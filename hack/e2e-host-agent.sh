#!/usr/bin/env bash
# End-to-end test of the metal-agent on Linux hosts. A kind cluster runs the
# operator; two hosts ("host-a" and "host-b") each run the agent in a
# container that shares a network namespace with a hailo-ollama simulator on
# the kind network, and each joins as a MetalNode in a namespace of its own.
# Checks:
#   - every MetalNode passes the dial-back,
#   - each host serves its own hailo:// InferenceService through the relay,
#     and only its own (Endpoints point at that host, logs never name the other),
#   - memory admission sizes a hailo:// source with a HEAD of its HEF on the
#     simulator's model library, before hailo-ollama pulls anything,
#   - a hailo:// Model on another accelerator is Failed with InvalidSource.
#
# Needs docker, kind, kubectl and go. KEEP=true leaves the cluster and
# containers up for inspection.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CLUSTER=${KIND_CLUSTER:-llmkube-host-agent-e2e}
IMG=${IMG:-example.com/llmkube:host-agent-e2e}
ROUTER_PROXY_IMG=${ROUTER_PROXY_IMG:-ghcr.io/defilantech/llmkube-router-proxy:dev}
HAILO_SIM_IMG=${HAILO_SIM_IMG:-localhost/llmkube-hailo-sim:dev}
KUSTOMIZE=${KUSTOMIZE:-$ROOT/bin/kustomize}
CURL_IMG=curlimages/curl:8.11.1
AGENT_BASE_IMG=gcr.io/distroless/static-debian12:latest
KEEP=${KEEP:-false}
HOSTS=(host-a host-b)

# tag is the hailo-ollama model each host serves.
tag() {
	case $1 in
	host-a) echo "qwen2.5-instruct:1.5b" ;;
	host-b) echo "llama3.2:3b" ;;
	esac
}

WORK=$(mktemp -d)
CREATED_CLUSTER=false

log() { printf '==> %s\n' "$*"; }

dump() {
	for h in "${HOSTS[@]}"; do
		echo "--- $h agent (last 40 lines)"
		docker logs --tail 40 "$h-agent" 2>&1 || true
	done
	kubectl get metalnode,model,isvc -A 2>&1 || true
}

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	dump >&2
	exit 1
}

cleanup() {
	if [ "$KEEP" = true ]; then
		log "KEEP=true: leaving cluster $CLUSTER, containers and $WORK"
		return
	fi
	for h in "${HOSTS[@]}"; do
		docker rm -f "$h-agent" "$h-engine" >/dev/null 2>&1 || true
	done
	if [ "$CREATED_CLUSTER" = true ]; then
		kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

# wait_for DESCRIPTION TIMEOUT_SECONDS COMMAND...
wait_for() {
	local what=$1 timeout=$2
	shift 2
	local end=$((SECONDS + timeout))
	until "$@" >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$end" ] || fail "timed out after ${timeout}s waiting for $what"
		sleep 3
	done
}

jsonpath_is() { # NAMESPACE RESOURCE JSONPATH WANT
	[ "$(kubectl -n "$1" get "$2" -o "jsonpath=$3" 2>/dev/null)" = "$4" ]
}

cd "$ROOT"

log "cluster $CLUSTER"
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
	kind create cluster --name "$CLUSTER"
	CREATED_CLUSTER=true
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

log "images"
make docker-build IMG="$IMG"
make docker-build-router-proxy ROUTER_PROXY_IMG="$ROUTER_PROXY_IMG"
make docker-build-hailo-sim HAILO_SIM_IMG="$HAILO_SIM_IMG"
kind load docker-image "$IMG" "$ROUTER_PROXY_IMG" --name "$CLUSTER"

log "operator"
make manifests >/dev/null
"$KUSTOMIZE" build config/default |
	sed -E "s#image: ghcr.io/defilantech/llmkube-controller:[^[:space:]]+#image: $IMG#" |
	kubectl apply --server-side --force-conflicts -f - >/dev/null
kubectl -n llmkube-system rollout status deploy/llmkube-controller-manager --timeout=300s

log "binaries"
CGO_ENABLED=0 GOOS=linux go build -o "$WORK/llmkube-metal-agent" ./cmd/metal-agent
go build -o "$WORK/llmkube" ./cmd/cli

CP_IP=$(docker inspect "$CLUSTER-control-plane" -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}')
CA=$(kubectl config view --raw --minify -o 'jsonpath={.clusters[0].cluster.certificate-authority-data}')

join_host() {
	local h=$1 dir="$WORK/$1"
	mkdir -p "$dir"
	kubectl create namespace "$h" >/dev/null
	"$WORK/llmkube" metal token create "$h" -n "$h" --server "https://$CP_IP:6443" --assume-same-server >/dev/null
	kubectl -n "$h" delete secret "llmkube-metal-$h-join" >/dev/null
	kubectl -n "$h" create token "llmkube-metal-$h" --duration=2h >"$dir/token"
	cat >"$dir/kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: e2e
  cluster:
    server: https://$CP_IP:6443
    certificate-authority-data: $CA
users:
- name: $h
  user:
    tokenFile: /host/token
contexts:
- name: $h
  context: {cluster: e2e, user: $h, namespace: $h}
current-context: $h
EOF
	chmod 600 "$dir/token" "$dir/kubeconfig"
	docker run -d --name "$h-engine" --hostname "$h" --network kind \
		-e HAILO_SIM_LISTEN=127.0.0.1:8000 -e HAILO_SIM_LIBRARY_LISTEN=127.0.0.1:8001 \
		"$HAILO_SIM_IMG" >/dev/null
	# The agent reads hailo-ollama's manifests to size hailo:// sources; on a
	# real host they are on its filesystem.
	docker cp "$h-engine:/usr/share/hailo-ollama/models/manifests" "$dir/manifests"
	docker run -d --name "$h-agent" --network "container:$h-engine" \
		-v "$dir:/host" -v "$WORK/llmkube-metal-agent:/usr/local/bin/llmkube-metal-agent:ro" \
		"$AGENT_BASE_IMG" llmkube-metal-agent \
		--kubeconfig /host/kubeconfig --namespace "$h" --metal-node "$h" \
		--renew-service-account-token --runtime ollama --ollama-port 8000 \
		--hailo-manifests-dir /host/manifests --hailo-library-url http://127.0.0.1:8001 \
		--allowed-download-hosts 127.0.0.1 >/dev/null
}

deploy_service() {
	local h=$1
	kubectl apply -f - >/dev/null <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: {name: $h-model, namespace: $h}
spec:
  source: hailo://$(tag "$h")
  hardware: {accelerator: metal}
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: {name: $h-svc, namespace: $h}
spec: {modelRef: $h-model, runtime: ollama, replicas: 1}
EOF
}

# chat HOST prints the reply to one chat completion through the relay. Each try
# runs curl in a pod and reads its log once the pod has finished, since
# kubectl run --attach can miss the output of a pod that exits quickly.
chat() {
	local h=$1 pod reply="" phase try _
	for try in 1 2 3; do
		pod="chat-$h-$try-$RANDOM"
		kubectl -n "$h" run "$pod" --restart=Never --image="$CURL_IMG" -- \
			curl -sS -m 60 "http://$h-svc.$h.svc.cluster.local:8080/v1/chat/completions" \
			-H 'Content-Type: application/json' \
			-d "{\"model\":\"$(tag "$h")\",\"messages\":[{\"role\":\"user\",\"content\":\"hello from $h\"}]}" \
			>/dev/null
		for _ in $(seq 1 60); do
			phase=$(kubectl -n "$h" get "pod/$pod" -o 'jsonpath={.status.phase}' 2>/dev/null || true)
			case "$phase" in Succeeded | Failed) break ;; esac
			sleep 2
		done
		reply=$(kubectl -n "$h" logs "pod/$pod" 2>&1 || true)
		kubectl -n "$h" delete pod "$pod" --wait=false >/dev/null 2>&1 || true
		case "$reply" in *hailo-sim:*)
			printf '%s' "$reply"
			return 0
			;;
		esac
		sleep 5
	done
	printf '%s' "$reply"
	return 1
}

log "hosts"
for h in "${HOSTS[@]}"; do join_host "$h"; done
for h in "${HOSTS[@]}"; do
	wait_for "MetalNode $h Reachable" 300 \
		jsonpath_is "$h" "metalnode/$h" '{.status.conditions[?(@.type=="Reachable")].status}' True
done

log "services"
for h in "${HOSTS[@]}"; do deploy_service "$h"; done
for h in "${HOSTS[@]}"; do
	wait_for "InferenceService $h/$h-svc Ready" 300 jsonpath_is "$h" "isvc/$h-svc" '{.status.phase}' Ready
done

log "chat through the relay"
for h in "${HOSTS[@]}"; do
	reply=$(chat "$h") || fail "chat with $h-svc failed: $reply"
	case "$reply" in
	*"\"model\":\"$(tag "$h")\""*"hailo-sim: hello from $h"*) ;;
	*) fail "unexpected reply from $h-svc: $reply" ;;
	esac
done

log "memory admission sizes hailo:// before anything is pulled"
for h in "${HOSTS[@]}"; do
	agent_log=$(docker logs "$h-agent" 2>&1)
	admitted=$(grep -n '"memory check passed"' <<<"$agent_log" | head -1 | cut -d: -f1)
	pulled=$(grep -n '"pulling Ollama model' <<<"$agent_log" | head -1 | cut -d: -f1)
	[ -n "$admitted" ] && [ -n "$pulled" ] && [ "$admitted" -lt "$pulled" ] ||
		fail "$h agent: memory check (line '$admitted') must pass before the first pull (line '$pulled')"
done

log "each host serves only its own namespace"
for h in "${HOSTS[@]}"; do
	host_ip=$(kubectl -n "$h" get "metalnode/$h" -o 'jsonpath={.status.hostIP}')
	eps=$(kubectl -n "$h" get endpointslices -l "kubernetes.io/service-name=$h-svc-agent" \
		-o 'jsonpath={.items[*].endpoints[*].addresses[*]}')
	[ -n "$host_ip" ] && [ "$eps" = "$host_ip" ] ||
		fail "$h-svc-agent endpoints are '$eps', want only MetalNode $h at '$host_ip'"
	for other in "${HOSTS[@]}"; do
		[ "$other" = "$h" ] && continue
		if docker logs "$h-agent" 2>&1 | grep -q "$other-svc"; then
			fail "$h agent log names $other-svc, a service outside its namespace"
		fi
	done
done

log "hailo:// on another accelerator is refused"
kubectl apply -f - >/dev/null <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: {name: hailo-on-cpu, namespace: host-a}
spec:
  source: hailo://qwen2.5-instruct:1.5b
  hardware: {accelerator: cpu}
EOF
wait_for "Model host-a/hailo-on-cpu Failed" 120 jsonpath_is host-a model/hailo-on-cpu '{.status.phase}' Failed
reasons=$(kubectl -n host-a get model/hailo-on-cpu -o 'jsonpath={.status.conditions[*].reason}')
case " $reasons " in
*" InvalidSource "*) ;;
*) fail "hailo-on-cpu conditions are '$reasons', want InvalidSource" ;;
esac

log "PASS"

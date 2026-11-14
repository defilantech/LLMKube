#!/usr/bin/env bash
# Copyright 2026 The LLMKube Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# kind-e2e-rbac.sh runs the operator under ONLY the RBAC generated from its
# +kubebuilder:rbac markers and fails on any forbidden API call (#2001).
#
# envtest and the admin kubeconfig most local testing uses never hit RBAC, and
# `make check-helm-rbac` only compares the chart with the markers. Neither can
# tell whether the markers cover every call the code makes. This script can:
#
#   1. create (or reuse) a kind cluster, with its own kubeconfig file so the
#      caller's default kubeconfig and context are never read or changed;
#   2. install the CRDs (config/crd plus the federation CRD the chart ships),
#      plus optional extra CRD directories and the upstream Gateway API CRDs;
#   3. render config/rbac with the repo's kustomize and bind a fresh
#      ServiceAccount to the generated ClusterRole (role.yaml) and the
#      leader-election Role, and nothing else;
#   4. mint a short-lived token for that ServiceAccount and run the manager
#      OUT OF CLUSTER with a kubeconfig holding only that token, so no image
#      build is needed;
#   5. apply fixtures (test/e2e-rbac/fixtures by default), watch for a bounded
#      window, delete the fixtures while the operator is still running (so
#      finalizer paths run too), and fail on a forbidden error or a reconcile
#      error loop.
#
# Forbidden errors are collected from three places: the operator log, the
# status of the fixture objects (some reconcilers, ModelRouter among them,
# write a failed API call into a condition and return nil, so it never
# reaches the log), and Events on LLMKube objects. The first forbidden error
# shortens the window to 20 more seconds, enough to collect the others.
#
# What the out-of-cluster run does not cover:
#   - admission webhooks (no serving cert, so cmd/main.go skips registering
#     them, and no ValidatingWebhookConfiguration is installed);
#   - the metrics endpoint and its authn/authz (metrics_auth_role.yaml),
#     disabled with --metrics-bind-address=0;
#   - anything that needs the operator's own pod: its Deployment, volume
#     mounts, and the in-cluster model cache path;
#   - reconcile paths the fixtures do not reach (GPU scheduling, gateway and
#     SLO integrations whose CRDs are absent, federation edge mode).
#
# Environment (all optional):
#   RBAC_E2E_CLUSTER              kind cluster name (default llmkube-rbac)
#   RBAC_E2E_KIND_NODE_IMAGE      kind node image when creating the cluster
#   RBAC_E2E_DELETE_CLUSTER       true deletes the cluster on exit (default false:
#                                 keep it for the next run, delete only what the
#                                 run created inside it)
#   RBAC_E2E_OPERATOR_NAMESPACE   namespace of the ServiceAccount and the
#                                 leader-election Lease (default llmkube-rbac-system)
#   RBAC_E2E_FIXTURES_NAMESPACE   namespace the fixtures go in (default llmkube-rbac-fixtures)
#   RBAC_E2E_FIXTURES_DIR         fixtures directory, applied recursively with
#                                 `kubectl apply -n <fixtures namespace>`
#   RBAC_E2E_WATCH_SECONDS        how long to watch after applying fixtures (default 180)
#   RBAC_E2E_TEARDOWN_SECONDS     how long to wait for fixture deletion (default 90)
#   RBAC_E2E_LOOP_THRESHOLD       fail when one object logs the same "Reconciler
#                                 error" this many times (default 10; 0 disables)
#   RBAC_E2E_EXTRA_CRD_DIRS       comma- or colon-separated CRD directories to
#                                 install too (a kustomization is built, otherwise
#                                 every manifest in the directory is applied)
#   RBAC_E2E_GATEWAY_API          true installs the upstream Gateway API standard CRDs
#   RBAC_E2E_GATEWAY_API_VERSION  pinned Gateway API release (default v1.6.0)
#   RBAC_E2E_OPERATOR_ARGS        extra manager flags (whitespace-separated)
#   RBAC_E2E_MANAGER_BIN          prebuilt manager binary for this host (skips go build)
#   RBAC_E2E_TOKEN_DURATION       ServiceAccount token lifetime (default 1h)
#   RBAC_E2E_WORKDIR              where the kubeconfigs, binary and operator log
#                                 go (default: a new mktemp directory)
#   KIND, KUBECTL, KUSTOMIZE      tool overrides (the Make target passes these)
#
# Exit status: 0 pass, 1 forbidden error / reconcile loop / operator exit,
# 2 setup failure.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

CLUSTER="${RBAC_E2E_CLUSTER:-llmkube-rbac}"
KIND_NODE_IMAGE="${RBAC_E2E_KIND_NODE_IMAGE:-}"
DELETE_CLUSTER="${RBAC_E2E_DELETE_CLUSTER:-false}"
OPERATOR_NS="${RBAC_E2E_OPERATOR_NAMESPACE:-llmkube-rbac-system}"
FIXTURES_NS="${RBAC_E2E_FIXTURES_NAMESPACE:-llmkube-rbac-fixtures}"
FIXTURES_DIR="${RBAC_E2E_FIXTURES_DIR:-${REPO_ROOT}/test/e2e-rbac/fixtures}"
WATCH_SECONDS="${RBAC_E2E_WATCH_SECONDS:-180}"
TEARDOWN_SECONDS="${RBAC_E2E_TEARDOWN_SECONDS:-90}"
LOOP_THRESHOLD="${RBAC_E2E_LOOP_THRESHOLD:-10}"
EXTRA_CRD_DIRS="${RBAC_E2E_EXTRA_CRD_DIRS:-}"
GATEWAY_API="${RBAC_E2E_GATEWAY_API:-false}"
GATEWAY_API_VERSION="${RBAC_E2E_GATEWAY_API_VERSION:-v1.6.0}"
OPERATOR_ARGS="${RBAC_E2E_OPERATOR_ARGS:-}"
MANAGER_BIN="${RBAC_E2E_MANAGER_BIN:-}"
TOKEN_DURATION="${RBAC_E2E_TOKEN_DURATION:-1h}"
WORKDIR="${RBAC_E2E_WORKDIR:-}"

KIND="${KIND:-kind}"
KUBECTL="${KUBECTL:-kubectl}"
KUSTOMIZE="${KUSTOMIZE:-${REPO_ROOT}/bin/kustomize}"

# Every cluster-scoped object this run creates carries this prefix (RBAC) or
# the llmkube.dev/rbac-e2e=true label (fixtures), so cleanup can find them.
NAME_PREFIX="llmkube-rbac-e2e-"
E2E_LABEL="llmkube.dev/rbac-e2e"
SA_NAME="${NAME_PREFIX}controller-manager"
LEASE_ID="882d6afe.llmkube.dev"
FORBIDDEN_RE='is forbidden|forbidden:'
FIXTURE_KINDS="models.inference.llmkube.dev,inferenceservices.inference.llmkube.dev,modelrouters.inference.llmkube.dev,modelpools.inference.llmkube.dev,loraadapters.inference.llmkube.dev,gpuquotas.inference.llmkube.dev"

OPERATOR_PID=""
CLEANED_UP=false

log() { printf '[rbac-e2e] %s\n' "$*" >&2; }
die() {
	log "ERROR: $*"
	exit 2
}

# k runs kubectl as cluster admin against the kind cluster only.
k() { "${KUBECTL}" --kubeconfig "${ADMIN_KUBECONFIG}" "$@"; }
# k_sa runs kubectl as the operator's ServiceAccount.
k_sa() { "${KUBECTL}" --kubeconfig "${SA_KUBECONFIG}" "$@"; }

require() {
	command -v "$1" >/dev/null 2>&1 || die "$1 not found on PATH${2:+ ($2)}"
}

preflight() {
	require "${KIND}" "install kind: https://kind.sigs.k8s.io"
	require "${KUBECTL}"
	require docker "kind needs a container runtime"
	require jq
	[[ -n "${MANAGER_BIN}" ]] || require go
	[[ -x "${KUSTOMIZE}" ]] || die "kustomize not found at ${KUSTOMIZE} (run 'make kustomize' or use 'make test-e2e-rbac')"
	[[ -d "${FIXTURES_DIR}" ]] || die "fixtures directory ${FIXTURES_DIR} does not exist"
	case "${WATCH_SECONDS}${TEARDOWN_SECONDS}${LOOP_THRESHOLD}" in
	*[!0-9]*) die "RBAC_E2E_WATCH_SECONDS, RBAC_E2E_TEARDOWN_SECONDS and RBAC_E2E_LOOP_THRESHOLD must be integers" ;;
	esac

	if [[ -z "${WORKDIR}" ]]; then
		WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/llmkube-rbac-e2e.XXXXXX")"
	fi
	mkdir -p "${WORKDIR}"
	WORKDIR="$(cd "${WORKDIR}" && pwd)"
	ADMIN_KUBECONFIG="${WORKDIR}/admin.kubeconfig"
	SA_KUBECONFIG="${WORKDIR}/operator-sa.kubeconfig"
	OPERATOR_LOG="${WORKDIR}/operator.log"
	STATUS_FINDINGS="${WORKDIR}/status-forbidden.txt"
	: >"${STATUS_FINDINGS}"
}

ensure_cluster() {
	if "${KIND}" get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
		log "reusing kind cluster ${CLUSTER}"
		(umask 077 && "${KIND}" get kubeconfig --name "${CLUSTER}" >"${ADMIN_KUBECONFIG}")
	else
		log "creating kind cluster ${CLUSTER}"
		local args=(create cluster --name "${CLUSTER}" --kubeconfig "${ADMIN_KUBECONFIG}" --wait 120s)
		[[ -n "${KIND_NODE_IMAGE}" ]] && args+=(--image "${KIND_NODE_IMAGE}")
		(umask 077 && "${KIND}" "${args[@]}")
	fi

	# Refuse anything that is not this kind cluster. The kubeconfig came from
	# kind itself, but check the two things a real cluster cannot fake by
	# accident: kind's context naming and kind's node providerID.
	local ctx provider
	ctx="$(k config current-context)"
	[[ "${ctx}" == "kind-${CLUSTER}" ]] || die "refusing to run: context ${ctx} is not kind-${CLUSTER}"
	provider="$(k get nodes -o jsonpath='{.items[0].spec.providerID}')"
	[[ "${provider}" == kind://* ]] || die "refusing to run: node providerID '${provider}' is not a kind node"
	log "cluster ok: context ${ctx}, node ${provider}"
}

apply_crd_dir() {
	local dir="$1"
	[[ -d "${dir}" ]] || die "CRD directory ${dir} does not exist"
	if [[ -f "${dir}/kustomization.yaml" || -f "${dir}/kustomization.yml" ]]; then
		"${KUSTOMIZE}" build "${dir}" | k apply --server-side --force-conflicts -f - >/dev/null
	else
		k apply --server-side --force-conflicts -R -f "${dir}" >/dev/null
	fi
}

install_crds() {
	log "installing CRDs (config/crd) and PriorityClasses"
	"${KUSTOMIZE}" build "${REPO_ROOT}/config/crd" | k apply --server-side --force-conflicts -f - >/dev/null

	# The PriorityClasses every install ships (config/manager, the chart's
	# priority-classes.yaml). Without them the inference pods are never
	# admitted and the pod-facing reconcile paths do not run. Like the CRDs,
	# they are left in place for the next run.
	k apply -f "${REPO_ROOT}/config/manager/priority_classes.yaml" >/dev/null

	if [[ "${GATEWAY_API}" == "true" ]]; then
		log "installing Gateway API ${GATEWAY_API_VERSION} standard CRDs"
		k apply --server-side --force-conflicts -f \
			"https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null
	fi

	if [[ -n "${EXTRA_CRD_DIRS}" ]]; then
		local dirs d
		IFS=',:' read -r -a dirs <<<"${EXTRA_CRD_DIRS}"
		for d in "${dirs[@]}"; do
			[[ -n "${d}" ]] || continue
			log "installing extra CRDs from ${d}"
			apply_crd_dir "${d}"
		done
	fi
	k wait --for=condition=Established crd --all --timeout=90s >/dev/null
}

# strip_finalizers clears finalizers on LLMKube objects left in a namespace,
# so a namespace can be deleted after the operator is gone.
strip_finalizers() {
	local ns="$1" res obj
	for res in models.inference.llmkube.dev inferenceservices.inference.llmkube.dev \
		modelrouters.inference.llmkube.dev modelpools.inference.llmkube.dev \
		loraadapters.inference.llmkube.dev gpuquotas.inference.llmkube.dev; do
		k get "${res}" -n "${ns}" -o name 2>/dev/null | while read -r obj; do
			[[ -n "${obj}" ]] || continue
			k patch "${obj}" -n "${ns}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
		done
	done
}

delete_namespace() {
	local ns="$1"
	k get namespace "${ns}" >/dev/null 2>&1 || return 0
	k delete namespace "${ns}" --wait=false >/dev/null 2>&1 || true
	if ! k wait --for=delete "namespace/${ns}" --timeout=60s >/dev/null 2>&1; then
		strip_finalizers "${ns}"
		k wait --for=delete "namespace/${ns}" --timeout=60s >/dev/null 2>&1 ||
			log "warning: namespace ${ns} is still terminating"
	fi
}

delete_cluster_scoped_leftovers() {
	if [[ -f "${WORKDIR}/rbac.yaml" ]]; then
		k delete --ignore-not-found -f "${WORKDIR}/rbac.yaml" >/dev/null 2>&1 || true
	fi
	k delete persistentvolumes,federatedclusters.federation.llmkube.dev -l "${E2E_LABEL}=true" \
		--ignore-not-found >/dev/null 2>&1 || true
}

render_rbac() {
	# Overlay the generated objects with the run's namespace and a prefix. Only
	# the ServiceAccount, the marker-generated ClusterRole and the
	# leader-election Role (and their bindings) are taken from config/rbac:
	# the metrics-auth and the admin/editor/viewer roles are not the
	# operator's permissions. kustomize rewrites the bindings' subjects and
	# roleRefs to the prefixed names and the namespace, the same way
	# config/default does for a real install.
	local overlay="${WORKDIR}/rbac-overlay"
	mkdir -p "${overlay}"
	cat >"${overlay}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: ${OPERATOR_NS}
namePrefix: ${NAME_PREFIX}
resources:
- ${REPO_ROOT}/config/rbac/service_account.yaml
- ${REPO_ROOT}/config/rbac/role.yaml
- ${REPO_ROOT}/config/rbac/role_binding.yaml
- ${REPO_ROOT}/config/rbac/leader_election_role.yaml
- ${REPO_ROOT}/config/rbac/leader_election_role_binding.yaml
EOF
	"${KUSTOMIZE}" build --load-restrictor LoadRestrictionsNone "${overlay}" >"${WORKDIR}/rbac.yaml"

	# Guard the rewrite: the ClusterRoleBinding must point at this run's
	# ServiceAccount, or the operator would run with no permissions (or with
	# someone else's) and the result would mean nothing.
	local subject
	subject="$(k create --dry-run=client -o json -f "${WORKDIR}/rbac.yaml" |
		jq -r 'if .kind == "List" then .items[] else . end
			| select(.kind == "ClusterRoleBinding") | .subjects[0] | "\(.namespace)/\(.name)"')"
	[[ "${subject}" == "${OPERATOR_NS}/${SA_NAME}" ]] ||
		die "rendered ClusterRoleBinding subject is '${subject}', want ${OPERATOR_NS}/${SA_NAME}"
}

setup_identity() {
	log "resetting namespaces ${OPERATOR_NS} and ${FIXTURES_NS}"
	delete_namespace "${FIXTURES_NS}"
	delete_namespace "${OPERATOR_NS}"
	delete_cluster_scoped_leftovers
	k create namespace "${OPERATOR_NS}" >/dev/null
	k create namespace "${FIXTURES_NS}" >/dev/null
	k label namespace "${OPERATOR_NS}" "${FIXTURES_NS}" "${E2E_LABEL}=true" >/dev/null

	log "binding ${OPERATOR_NS}/${SA_NAME} to the generated ClusterRole and leader-election Role"
	render_rbac
	k apply -f "${WORKDIR}/rbac.yaml" >/dev/null

	local token server ca
	token="$(k create token "${SA_NAME}" -n "${OPERATOR_NS}" --duration="${TOKEN_DURATION}")"
	server="$(k config view --raw --minify -o jsonpath='{.clusters[0].cluster.server}')"
	ca="$(k config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
	(
		umask 077
		cat >"${SA_KUBECONFIG}" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: kind-${CLUSTER}
  cluster:
    server: ${server}
    certificate-authority-data: ${ca}
users:
- name: ${SA_NAME}
  user:
    token: ${token}
contexts:
- name: rbac-e2e
  context:
    cluster: kind-${CLUSTER}
    user: ${SA_NAME}
current-context: rbac-e2e
EOF
	)

	local who
	who="$(k_sa auth whoami -o jsonpath='{.status.userInfo.username}')"
	[[ "${who}" == "system:serviceaccount:${OPERATOR_NS}:${SA_NAME}" ]] ||
		die "operator kubeconfig authenticates as '${who}', not the ServiceAccount"
	if k_sa auth can-i '*' '*' --all-namespaces >/dev/null 2>&1; then
		die "the ServiceAccount can do anything; some other binding grants it cluster-admin, so the run would prove nothing"
	fi
	log "operator identity: ${who}"
}

build_manager() {
	if [[ -n "${MANAGER_BIN}" ]]; then
		[[ -x "${MANAGER_BIN}" ]] || die "RBAC_E2E_MANAGER_BIN ${MANAGER_BIN} is not executable"
		return
	fi
	log "building the manager for this host"
	MANAGER_BIN="${WORKDIR}/manager"
	(cd "${REPO_ROOT}" && go build -o "${MANAGER_BIN}" ./cmd)
}

operator_running() {
	[[ -n "${OPERATOR_PID}" ]] && kill -0 "${OPERATOR_PID}" 2>/dev/null
}

start_operator() {
	local extra=()
	[[ -n "${OPERATOR_ARGS}" ]] && read -r -a extra <<<"${OPERATOR_ARGS}"
	mkdir -p "${WORKDIR}/models"
	log "starting the operator out of cluster (log: ${OPERATOR_LOG})"
	# Webhooks stay off (no --webhook-cert-path), metrics and probes bind
	# nowhere, and leader election is on so the leader-election Role is used.
	# JSON logs keep each record on one line for the scan below.
	env -u OTEL_EXPORTER_OTLP_ENDPOINT KUBECONFIG="${SA_KUBECONFIG}" \
		"${MANAGER_BIN}" \
		--leader-elect \
		--leader-election-namespace="${OPERATOR_NS}" \
		--metrics-bind-address=0 \
		--health-probe-bind-address=0 \
		--model-cache-path="${WORKDIR}/models" \
		--zap-devel=false \
		--zap-encoder=json \
		--zap-stacktrace-level=panic \
		${extra[@]+"${extra[@]}"} \
		>"${OPERATOR_LOG}" 2>&1 &
	OPERATOR_PID=$!

	local waited=0
	until grep -qi 'successfully acquired lease' "${OPERATOR_LOG}"; do
		operator_running || return 1
		((waited >= 120)) && return 1
		sleep 2
		waited=$((waited + 2))
	done
	log "operator is leader (lease ${OPERATOR_NS}/${LEASE_ID})"
}

stop_operator() {
	operator_running || return 0
	kill -TERM "${OPERATOR_PID}" 2>/dev/null || true
	local waited=0
	while operator_running && ((waited < 10)); do
		sleep 1
		waited=$((waited + 1))
	done
	if operator_running; then
		kill -KILL "${OPERATOR_PID}" 2>/dev/null || true
	fi
	wait "${OPERATOR_PID}" 2>/dev/null || true
}

cleanup() {
	local status=$?
	[[ "${CLEANED_UP}" == "true" ]] && return
	CLEANED_UP=true
	set +e
	stop_operator
	if [[ -n "${ADMIN_KUBECONFIG:-}" && -f "${ADMIN_KUBECONFIG}" ]]; then
		if [[ "${DELETE_CLUSTER}" == "true" ]]; then
			log "deleting kind cluster ${CLUSTER}"
			"${KIND}" delete cluster --name "${CLUSTER}" --kubeconfig "${ADMIN_KUBECONFIG}" >/dev/null 2>&1
		else
			log "cleaning up (keeping kind cluster ${CLUSTER}; RBAC_E2E_DELETE_CLUSTER=true removes it)"
			delete_namespace "${FIXTURES_NS}"
			delete_namespace "${OPERATOR_NS}"
			delete_cluster_scoped_leftovers
		fi
	fi
	rm -f "${SA_KUBECONFIG:-}" "${ADMIN_KUBECONFIG:-}"
	[[ -n "${OPERATOR_LOG:-}" && -f "${OPERATOR_LOG}" ]] && log "operator log kept at ${OPERATOR_LOG}"
	exit "${status}"
}

# scan_status records forbidden messages that never reach the operator log.
# Some reconcilers (ModelRouter, for one) write a failed API call into a status
# condition or a Warning Event and return nil, so the log scan alone would
# miss them. Each line is "<source><TAB><message>".
scan_status() {
	{
		k get "${FIXTURE_KINDS}" -n "${FIXTURES_NS}" -o json 2>/dev/null
		k get federatedclusters.federation.llmkube.dev -l "${E2E_LABEL}=true" -o json 2>/dev/null
	} | jq -r --arg re "${FORBIDDEN_RE}" '.items[] | . as $o
		| [(.status // {}) | .. | strings | select(test($re))] | unique[]
		| "status(\($o.kind)/\($o.metadata.name))\t\(.)"' >>"${STATUS_FINDINGS}" 2>/dev/null || true
	k get events -n "${FIXTURES_NS}" -o json 2>/dev/null |
		jq -r --arg re "${FORBIDDEN_RE}" '.items[]
		| select((.involvedObject.apiVersion // "") | test("llmkube[.]dev/"))
		| select((.message // "") | test($re))
		| "event(\(.involvedObject.kind)/\(.involvedObject.name))\t\(.message)"' >>"${STATUS_FINDINGS}" 2>/dev/null || true
}

forbidden_count() {
	local n m
	n="$(grep -cE "${FORBIDDEN_RE}" "${OPERATOR_LOG}" 2>/dev/null || true)"
	m="$(wc -l <"${STATUS_FINDINGS}" | tr -d ' ')"
	echo $((${n:-0} + ${m:-0}))
}

watch_window() {
	local deadline now grace_set=false
	deadline=$(($(date +%s) + WATCH_SECONDS))
	log "watching the operator for ${WATCH_SECONDS}s"
	while :; do
		now=$(date +%s)
		((now >= deadline)) && break
		if ! operator_running; then
			OPERATOR_EXITED=true
			return
		fi
		# After the first forbidden line, keep watching briefly to collect
		# the rest of the gaps, then stop early instead of burning the window.
		scan_status
		if [[ "${grace_set}" == "false" && "$(forbidden_count)" -gt 0 ]]; then
			grace_set=true
			((now + 20 < deadline)) && deadline=$((now + 20))
			log "forbidden error seen; collecting for up to 20s more"
		fi
		sleep 5
	done
}

teardown_fixtures() {
	log "deleting fixtures with the operator still running (finalizer paths)"
	if ! k delete -n "${FIXTURES_NS}" -R -f "${FIXTURES_DIR}" --ignore-not-found \
		--wait=true --timeout="${TEARDOWN_SECONDS}s" >/dev/null 2>&1; then
		log "warning: fixtures were not all gone after ${TEARDOWN_SECONDS}s (a finalizer was not released)"
		k get models,inferenceservices,modelrouters,modelpools,loraadapters,gpuquotas \
			-n "${FIXTURES_NS}" 2>/dev/null | sed 's/^/    /' >&2
	fi
	sleep 3
}

# report_forbidden prints one line per distinct (source, verb, resource,
# group, scope), from the operator log and from status conditions and Events,
# and returns non-zero when there is at least one.
report_forbidden() {
	local lines
	lines="$(
		grep -E "${FORBIDDEN_RE}" "${OPERATOR_LOG}" | sed 's/\\"/"/g' | awk '{ print "log\t" $0 }' || true
		sort -u "${STATUS_FINDINGS}"
	)"
	[[ -n "${lines}" ]] || return 0

	local rbac_re='[Uu]ser "([^"]+)" cannot ([a-z]+) resource "([^"]+)" in API group "([^"]*)"( in the namespace "([^"]+)"| at the cluster scope)?'
	local ctrl_re='"controller":"([^"]+)"'
	local type_re='type="([^"]+)"'
	local origin line source verb resource group scope rows="" others=""
	while IFS=$'\t' read -r origin line; do
		source="${origin}"
		if [[ "${origin}" == "log" ]]; then
			source="log:unknown"
			if [[ "${line}" =~ ${ctrl_re} ]]; then
				source="log:controller=${BASH_REMATCH[1]}"
			elif [[ "${line}" =~ ${type_re} ]]; then
				source="log:informer(${BASH_REMATCH[1]})"
			elif [[ "${line}" == *"event"* ]]; then
				source="log:event-recorder"
			fi
		fi
		if [[ "${line}" =~ ${rbac_re} ]]; then
			verb="${BASH_REMATCH[2]}"
			resource="${BASH_REMATCH[3]}"
			group="${BASH_REMATCH[4]}"
			scope="cluster"
			[[ -n "${BASH_REMATCH[6]}" ]] && scope="namespace ${BASH_REMATCH[6]}"
			rows+="${source}|${verb}|${resource}|${group}|${scope}"$'\n'
		else
			others+="${source}|$(printf '%s' "${line}" | grep -oE "[^\"]*(${FORBIDDEN_RE})[^\"]*" | head -1 | cut -c1-300)"$'\n'
		fi
	done <<<"${lines}"

	log "FAIL: forbidden API calls under the generated RBAC:"
	if [[ -n "${rows}" ]]; then
		printf '%s' "${rows}" | sort | uniq -c | sort -rn |
			while read -r count row; do
				IFS='|' read -r source verb resource group scope <<<"${row}"
				printf '  %s: verb=%s resource=%s group="%s" scope=%s (x%s)\n' \
					"${source}" "${verb}" "${resource}" "${group}" "${scope}" "${count}" >&2
			done
		log "missing permissions, as markers:"
		printf '%s' "${rows}" | cut -d'|' -f2-4 | sort -u |
			while IFS='|' read -r verb resource group; do
				printf '  // +kubebuilder:rbac:groups=%s,resources=%s,verbs=%s\n' \
					"${group:-core}" "${resource}" "${verb}" >&2
			done
	fi
	if [[ -n "${others}" ]]; then
		printf '%s' "${others}" | sort -u | head -10 | while IFS='|' read -r source msg; do
			printf '  %s: %s\n' "${source}" "${msg}" >&2
		done
	fi
	return 1
}

# report_loops flags the same object failing with the same "Reconciler error"
# at least LOOP_THRESHOLD times. controller-runtime retries a failed reconcile
# with exponential backoff from 5ms, so an error that persists for more than a
# few seconds logs well over 10 times inside the window, while a transient
# error logs once or twice. Optimistic-concurrency conflicts ("the object has
# been modified") are normal and ignored; forbidden errors are reported above.
report_loops() {
	((LOOP_THRESHOLD > 0)) || return 0
	local loops
	loops="$(jq -R -r 'fromjson? | select(.msg == "Reconciler error")
		| select((.error // "") | test("the object has been modified|is forbidden") | not)
		| [(.controller // "?"), ((.namespace // "") + "/" + (.name // "")), ((.error // "") | .[0:240])]
		| @tsv' "${OPERATOR_LOG}" | sort | uniq -c | awk -v n="${LOOP_THRESHOLD}" '$1 >= n' | sort -rn)"
	[[ -n "${loops}" ]] || return 0
	log "FAIL: reconcile error loops (same object, same error, >= ${LOOP_THRESHOLD} times):"
	printf '%s\n' "${loops}" | while IFS=$'\t' read -r head object err; do
		printf '  %s object=%s\n      error: %s\n' "${head}" "${object}" "${err}" >&2
	done
	return 1
}

print_activity() {
	log "reconcile activity by controller (log records, startup and shutdown excluded):"
	jq -R -r 'fromjson? | select((.msg // "") | test("^Starting |Shutdown signal|All workers finished") | not)
		| .controller // empty' "${OPERATOR_LOG}" | sort | uniq -c | sort -rn |
		sed 's/^/    /' >&2
}

main() {
	preflight
	trap cleanup EXIT
	trap 'exit 130' INT TERM
	local start
	start=$(date +%s)

	ensure_cluster
	install_crds
	build_manager
	setup_identity

	OPERATOR_EXITED=false
	if ! start_operator; then
		report_forbidden || true
		tail -n 40 "${OPERATOR_LOG}" | sed 's/^/    /' >&2
		log "FAIL: the operator did not acquire its leader-election lease"
		exit 1
	fi

	log "applying fixtures from ${FIXTURES_DIR} to ${FIXTURES_NS}"
	k apply -n "${FIXTURES_NS}" -R -f "${FIXTURES_DIR}" >/dev/null
	watch_window

	scan_status
	log "fixture state after the watch window:"
	k get models,inferenceservices,modelrouters,modelpools,loraadapters,gpuquotas \
		-n "${FIXTURES_NS}" 2>/dev/null | sed 's/^/    /' >&2 || true

	if [[ "${OPERATOR_EXITED}" == "false" ]]; then
		teardown_fixtures
		operator_running || OPERATOR_EXITED=true
	fi

	print_activity
	local failed=false
	report_forbidden || failed=true
	report_loops || failed=true
	if [[ "${OPERATOR_EXITED}" == "true" ]]; then
		tail -n 40 "${OPERATOR_LOG}" | sed 's/^/    /' >&2
		log "FAIL: the operator exited during the run"
		failed=true
	fi

	local elapsed=$(($(date +%s) - start))
	if [[ "${failed}" == "true" ]]; then
		log "RESULT: FAIL (${elapsed}s)"
		exit 1
	fi
	log "RESULT: PASS: no forbidden errors or reconcile loops under the generated RBAC (${elapsed}s)"
}

main "$@"

#!/usr/bin/env bash
# Run against a disposable cluster with the updated operator already installed.
# The registry must be reachable from cluster pods and nodes. No GPU is needed.
set -euo pipefail
: "${KUBECONFIG:?Set KUBECONFIG to a disposable cluster with ImageVolume support}"
: "${OCI_PREFETCH_IMAGE:?Set OCI_PREFETCH_IMAGE to a writable registry/repository:unique-tag}"
ns="oci-prefetch-test-${RANDOM}"
cleanup() {
  result=$?
  if (( result != 0 )); then
    kubectl -n "$ns" get models,inferenceservices,pods || true
    kubectl -n "$ns" get events --sort-by=.lastTimestamp || true
    kubectl -n "$ns" logs job/weights-prefetch-oci --all-containers=true || true
    kubectl -n "$ns" logs deployment/served --all-containers=true || true
    kubectl -n "$ns" logs bytes-check || true
  fi
  kubectl delete namespace "$ns" --wait=false >/dev/null || true
  return "$result"
}
kubectl create namespace "$ns"
trap cleanup EXIT
secret_spec=""
pull_spec=""
if [[ -n "${OCI_PREFETCH_DOCKER_CONFIG:-}" ]]; then
  kubectl -n "$ns" create secret generic registry-auth \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=".dockerconfigjson=${OCI_PREFETCH_DOCKER_CONFIG}"
  # The Job uses this namespace's default ServiceAccount to pull the operator
  # image; use the supplied config for private publisher images as well.
  kubectl -n "$ns" patch serviceaccount default --type=merge \
    -p '{"imagePullSecrets":[{"name":"registry-auth"}]}'
  secret_spec=$'    pushSecretRef:\n      name: registry-auth'
  pull_spec=$'  imagePullSecrets:\n    - name: registry-auth'
fi
kubectl -n "$ns" create configmap weights \
  --from-literal=weights.gguf=oci-prefetch-fixture \
  --from-literal=second.gguf=oci-prefetch-second-shard \
  --from-literal=projector.gguf=oci-prefetch-projector
kubectl -n "$ns" apply -f - <<EOF_MANIFEST
apiVersion: v1
kind: Pod
metadata:
  name: source
  labels:
    app: source
spec:
  containers:
    - name: http
      image: python:3.13-alpine
      command: [python, -m, http.server, "8080", --directory, /fixtures]
      ports:
        - containerPort: 8080
      readinessProbe:
        httpGet:
          path: /weights.gguf
          port: 8080
      volumeMounts:
        - name: weights
          mountPath: /fixtures
  volumes:
    - name: weights
      configMap:
        name: weights
---
apiVersion: v1
kind: Service
metadata:
  name: source
spec:
  selector:
    app: source
  ports:
    - port: 8080
      targetPort: 8080
EOF_MANIFEST
kubectl -n "$ns" wait pod/source --for=condition=Ready --timeout=180s
# Multi-file HTTP inputs use the existing HF mirror path. Explicit files need
# no repository listing; the fixture serves each relative file directly.
kubectl -n "$ns" create secret generic source-endpoint \
  --from-literal="HF_ENDPOINT=http://source.${ns}.svc.cluster.local:8080"
kubectl -n "$ns" apply -f - <<EOF_MODEL
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: weights
spec:
  source: http://source.${ns}.svc.cluster.local:8080
  sourceSecretRef:
    name: source-endpoint
  files: [weights.gguf, second.gguf]
  mmproj: projector.gguf
  format: gguf
  prefetch: true
  prefetchOCI:
    image: ${OCI_PREFETCH_IMAGE}
    ephemeralStorage: 1Gi
${secret_spec}
EOF_MODEL
if ! kubectl -n "$ns" wait model/weights --for=jsonpath='{.status.prefetchOCI.phase}'=Succeeded --timeout=300s; then
  kubectl -n "$ns" get model/weights -o yaml
  kubectl -n "$ns" get pods
  kubectl -n "$ns" logs job/weights-prefetch-oci --all-containers=true || true
  exit 1
fi
image=$(kubectl -n "$ns" get model weights -o jsonpath='{.status.prefetchOCI.image}')
files=$(kubectl -n "$ns" get model weights -o jsonpath='{.status.prefetchOCI.files[0]}')
[[ "$image" == oci://*@sha256:* && "$files" == weights.gguf ]]
[[ -z "$(kubectl -n "$ns" get pvc -o name)" ]]
printf 'Published: %s\n' "$image"
# Remove the provider before consuming the artifact: no source download can pass.
kubectl -n "$ns" delete pod/source service/source
kubectl -n "$ns" apply -f - <<EOF_PROBE
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: promoted
spec:
  source: ${image}
  format: gguf
  files: [weights.gguf, second.gguf]
  mmproj: projector.gguf
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: served
spec:
  modelRef: promoted
  runtime: generic
  stageModel: true
  image: python:3.13-alpine
  command: [python, -m, http.server]
  args: ["8080", --directory, /model-source]
  containerPort: 8080
  endpoint:
    port: 8080
    type: ClusterIP
${pull_spec}
EOF_PROBE
kubectl -n "$ns" wait model/promoted --for=jsonpath='{.status.phase}'=Ready --timeout=120s
kubectl -n "$ns" wait inferenceservice/served --for=jsonpath='{.status.phase}'=Ready --timeout=300s
# Inspect the operator's serving Deployment; a handwritten ImageVolume Pod
# would not catch a regression in the existing oci:// serving path.
mounted=$(kubectl -n "$ns" get deployment served -o jsonpath='{.spec.template.spec.volumes[?(@.name=="model-source")].image.reference}')
[[ "$mounted" == "${image#oci://}" ]]
[[ -z "$(kubectl -n "$ns" get pvc -o name)" ]]
printf 'Serving through operator-created ImageVolume: %s\n' "$mounted"
kubectl -n "$ns" apply -f - <<'EOF_CHECK'
apiVersion: v1
kind: Pod
metadata:
  name: bytes-check
spec:
  restartPolicy: Never
  containers:
    - name: check
      image: python:3.13-alpine
      command: [python, -c]
      args:
        - |
          from urllib.request import urlopen
          expected = {
              "weights.gguf": b"oci-prefetch-fixture",
              "second.gguf": b"oci-prefetch-second-shard",
              "projector.gguf": b"oci-prefetch-projector",
          }
          for name, want in expected.items():
              with urlopen("http://served:8080/" + name, timeout=10) as response:
                  got = response.read()
              assert got == want, (name, got, want)
              print(f"BYTES CHECK PASSED: {name} = {got!r}", flush=True)
EOF_CHECK
kubectl -n "$ns" wait pod/bytes-check --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
kubectl -n "$ns" logs bytes-check
printf 'OCI prefetch smoke passed: %s\n' "$image"
# This intentionally leaves the published tag in the registry for inspection.

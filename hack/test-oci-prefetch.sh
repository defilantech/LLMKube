#!/usr/bin/env bash
# Run against a disposable cluster with the updated operator already installed.
# The registry must be reachable from cluster pods and nodes. No GPU is needed.
set -euo pipefail
: "${KUBECONFIG:?Set KUBECONFIG to a disposable cluster with ImageVolume support}"
: "${OCI_PREFETCH_IMAGE:?Set OCI_PREFETCH_IMAGE to a writable registry/repository:unique-tag}"
ns="oci-prefetch-test-${RANDOM}"
cleanup() { kubectl delete namespace "$ns" --wait=false >/dev/null; }
kubectl create namespace "$ns"
trap cleanup EXIT
secret_spec=""
pull_spec=""
if [[ -n "${OCI_PREFETCH_DOCKER_CONFIG:-}" ]]; then
  kubectl -n "$ns" create secret generic registry-auth \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=".dockerconfigjson=${OCI_PREFETCH_DOCKER_CONFIG}"
  secret_spec=$'    pushSecretRef:\n      name: registry-auth'
  pull_spec=$'  imagePullSecrets:\n    - name: registry-auth'
fi
kubectl -n "$ns" create configmap weights --from-literal=weights.gguf=oci-prefetch-fixture
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
kubectl -n "$ns" apply -f - <<EOF_MODEL
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: weights
spec:
  source: http://source.${ns}.svc.cluster.local:8080/weights.gguf
  format: gguf
  prefetch: true
  prefetchOCI:
    image: ${OCI_PREFETCH_IMAGE}
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
  files: [weights.gguf]
---
apiVersion: v1
kind: Pod
metadata:
  name: volume-probe
spec:
  restartPolicy: Never
${pull_spec}
  containers:
    - name: probe
      image: python:3.13-alpine
      command:
        - python
        - -c
        - "from pathlib import Path; assert Path('/model-source/weights.gguf').read_bytes() == b'oci-prefetch-fixture'"
      volumeMounts:
        - name: model
          mountPath: /model-source
          readOnly: true
  volumes:
    - name: model
      image:
        reference: ${image#oci://}
        pullPolicy: IfNotPresent
EOF_PROBE
kubectl -n "$ns" wait model/promoted --for=jsonpath='{.status.phase}'=Ready --timeout=120s
kubectl -n "$ns" wait pod/volume-probe --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
printf 'OCI prefetch smoke passed: %s\n' "$image"
# This intentionally leaves the published tag in the registry for inspection.

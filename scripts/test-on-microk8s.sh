#!/bin/bash
# Test feature branches on MicroK8s cluster
# Usage: ./scripts/test-on-microk8s.sh [image-tag] [cluster-host]
set -e

# Configuration
IMAGE_TAG="${1:-test}"
CLUSTER_HOST="${2:-shadowstack}"
IMAGE_NAME="llmkube-controller"

echo "========================================"
echo "Testing LLMKube on ${CLUSTER_HOST}"
echo "Image: ${IMAGE_NAME}:${IMAGE_TAG}"
echo "========================================"

echo ""
echo "=== Step 1: Building Docker image for linux/amd64 ==="
docker buildx build --platform linux/amd64 -t ${IMAGE_NAME}:${IMAGE_TAG} --load .

echo ""
echo "=== Step 2: Transferring image to ${CLUSTER_HOST} ==="
docker save ${IMAGE_NAME}:${IMAGE_TAG} -o /tmp/${IMAGE_NAME}-${IMAGE_TAG}.tar
scp /tmp/${IMAGE_NAME}-${IMAGE_TAG}.tar ${CLUSTER_HOST}:/tmp/

echo ""
echo "=== Step 3: Importing image into MicroK8s containerd ==="
ssh ${CLUSTER_HOST} "microk8s ctr image import /tmp/${IMAGE_NAME}-${IMAGE_TAG}.tar"

echo ""
echo "=== Step 4: Updating controller deployment ==="
kubectl patch deployment llmkube-controller-manager -n llmkube-system \
  --type='json' \
  -p='[
    {"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "docker.io/library/'${IMAGE_NAME}':'${IMAGE_TAG}'"},
    {"op": "replace", "path": "/spec/replicas", "value": 1}
  ]'

echo ""
echo "=== Step 5: Waiting for rollout ==="
kubectl rollout status deployment/llmkube-controller-manager -n llmkube-system --timeout=120s

echo ""
echo "=== Step 6: Verification ==="
echo "Controller pod:"
kubectl get pods -n llmkube-system -l control-plane=controller-manager

echo ""
echo "Controller logs (last 15 lines):"
kubectl logs -n llmkube-system -l control-plane=controller-manager --tail=15

echo ""
echo "========================================"
echo "SUCCESS! Controller is running with ${IMAGE_NAME}:${IMAGE_TAG}"
echo "========================================"
echo ""
echo "Next steps:"
echo "  1. Deploy test resources: kubectl apply -f your-test.yaml"
echo "  2. Watch controller logs: kubectl logs -n llmkube-system -l control-plane=controller-manager -f"
echo "  3. To restore original: helm upgrade llmkube llmkube/llmkube -n llmkube-system"

# Cleanup temp files
rm -f /tmp/${IMAGE_NAME}-${IMAGE_TAG}.tar
ssh ${CLUSTER_HOST} "rm -f /tmp/${IMAGE_NAME}-${IMAGE_TAG}.tar" 2>/dev/null || true

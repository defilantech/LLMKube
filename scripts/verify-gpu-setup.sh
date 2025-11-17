#!/bin/bash
# GPU Setup Verification Script
# Runs all checks to verify GPU cluster is ready for LLMKube

set -e

echo "🔍 LLMKube GPU Setup Verification"
echo "=================================="
echo ""

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Check functions
check_pass() {
    echo -e "${GREEN}✅ $1${NC}"
}

check_fail() {
    echo -e "${RED}❌ $1${NC}"
}

check_warn() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

# 1. Check kubectl connection
echo "1️⃣  Checking kubectl connection..."
if kubectl cluster-info &> /dev/null; then
    check_pass "kubectl is connected"
    CLUSTER=$(kubectl config current-context)
    echo "   Current cluster: $CLUSTER"
else
    check_fail "kubectl not connected. Run: gcloud container clusters get-credentials llmkube-gpu-cluster --region us-central1"
    exit 1
fi
echo ""

# 2. Check for GPU nodes
echo "2️⃣  Checking for GPU nodes..."
GPU_NODES=$(kubectl get nodes -l cloud.google.com/gke-accelerator=nvidia-tesla-t4 --no-headers 2>/dev/null | wc -l)
if [ $GPU_NODES -gt 0 ]; then
    check_pass "Found $GPU_NODES GPU node(s)"
    kubectl get nodes -l cloud.google.com/gke-accelerator=nvidia-tesla-t4 -o custom-columns=NAME:.metadata.name,STATUS:.status.conditions[-1].type,GPU:.status.allocatable."nvidia\.com/gpu"
elif [ $GPU_NODES -eq 0 ]; then
    check_warn "No GPU nodes currently running (auto-scaled to 0)"
    echo "   This is normal for cost savings. GPU nodes will scale up when needed."
else
    check_fail "GPU node pool may not be configured"
    exit 1
fi
echo ""

# 3. Check NVIDIA device plugin
echo "3️⃣  Checking NVIDIA device plugin..."
DEVICE_PLUGIN=$(kubectl get daemonset -n kube-system -l k8s-app=nvidia-gpu-device-plugin --no-headers 2>/dev/null | wc -l)
if [ $DEVICE_PLUGIN -gt 0 ]; then
    check_pass "NVIDIA device plugin daemonset found"
    kubectl get daemonset -n kube-system -l k8s-app=nvidia-gpu-device-plugin
else
    check_warn "NVIDIA device plugin not found"
    echo "   Installing device plugin..."
    kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/container-engine-accelerators/master/nvidia-driver-installer/cos/daemonset-preloaded.yaml
    check_pass "Device plugin installed"
fi
echo ""

# 4. Check LLMKube CRDs
echo "4️⃣  Checking LLMKube CRDs..."
MODEL_CRD=$(kubectl get crd models.inference.llmkube.dev --no-headers 2>/dev/null | wc -l)
ISVC_CRD=$(kubectl get crd inferenceservices.inference.llmkube.dev --no-headers 2>/dev/null | wc -l)

if [ $MODEL_CRD -gt 0 ] && [ $ISVC_CRD -gt 0 ]; then
    check_pass "LLMKube CRDs installed"
    kubectl get crd | grep llmkube
else
    check_fail "LLMKube CRDs not installed. Run: make install"
    exit 1
fi
echo ""

# 5. Check LLMKube controller
echo "5️⃣  Checking LLMKube controller..."
CONTROLLER=$(kubectl get deployment -n llmkube-system llmkube-controller-manager --no-headers 2>/dev/null | wc -l)
if [ $CONTROLLER -gt 0 ]; then
    check_pass "Controller deployed"
    kubectl get pods -n llmkube-system

    # Check controller logs for GPU support
    POD=$(kubectl get pod -n llmkube-system -l control-plane=controller-manager -o jsonpath='{.items[0].metadata.name}')
    if kubectl logs -n llmkube-system $POD --tail=10 | grep -q "Starting Controller"; then
        check_pass "Controller is running"
    fi
else
    check_warn "Controller not deployed. Run: make deploy IMG=gcr.io/llmkube-478121/llmkube-controller:latest"
fi
echo ""

# 6. Test GPU scheduling (optional)
echo "6️⃣  Testing GPU scheduling..."
read -p "Deploy test GPU pod? (y/N): " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo "   Deploying test pod (may take 2-3 min if scaling GPU node)..."

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test-verify
spec:
  restartPolicy: OnFailure
  containers:
  - name: cuda-test
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["sh", "-c", "nvidia-smi && echo 'GPU Test Successful!'"]
    resources:
      limits:
        nvidia.com/gpu: 1
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
  nodeSelector:
    cloud.google.com/gke-accelerator: nvidia-tesla-t4
EOF

    echo "   Waiting for pod to complete (timeout: 5min)..."
    kubectl wait --for=condition=Ready pod/gpu-test-verify --timeout=300s 2>/dev/null || \
        check_warn "Pod still pending. Check: kubectl describe pod gpu-test-verify"

    # Check logs
    sleep 5
    if kubectl logs gpu-test-verify 2>/dev/null | grep -q "Tesla T4"; then
        check_pass "GPU test successful! T4 GPU detected"
        kubectl logs gpu-test-verify
    else
        check_warn "GPU test pending or failed"
        kubectl describe pod gpu-test-verify | tail -20
    fi

    # Clean up
    read -p "Delete test pod? (Y/n): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Nn]$ ]]; then
        kubectl delete pod gpu-test-verify
        check_pass "Test pod deleted"
    fi
else
    echo "   Skipping GPU test"
fi
echo ""

# 7. Summary and next steps
echo "=========================================="
echo "📊 Summary"
echo "=========================================="

if [ $GPU_NODES -gt 0 ]; then
    echo "✅ GPU cluster is ready!"
    echo ""
    echo "Next steps:"
    echo "  1. Deploy a GPU model: kubectl apply -f config/samples/gpu-model-example.yaml"
    echo "  2. Monitor costs: See docs/gcp-billing-alerts.md"
    echo "  3. Run benchmarks: See docs/gpu-setup-guide.md"
else
    echo "⚠️  GPU nodes are auto-scaled to 0 (cost saving mode)"
    echo ""
    echo "GPU nodes will automatically scale up when you deploy a GPU workload."
    echo "To test immediately, deploy: config/samples/gpu-model-example.yaml"
fi

echo ""
echo "📚 Documentation:"
echo "  - GPU Setup Guide: docs/gpu-setup-guide.md"
echo "  - Billing Alerts: docs/gcp-billing-alerts.md"
echo "  - Quick Start: terraform/gke/quick-start.sh"
echo ""
echo "💰 Cost Management:"
echo "  - Current GPU nodes: $GPU_NODES"
echo "  - Estimated daily cost: \$$(echo "$GPU_NODES * 8.40" | bc) (T4 spot)"
echo "  - Monthly projection: \$$(echo "$GPU_NODES * 252" | bc)"
echo ""
echo "  To save costs when idle:"
echo "    kubectl delete inferenceservices --all"
echo "    # GPU nodes will auto-scale to 0 after ~10 minutes"
echo ""

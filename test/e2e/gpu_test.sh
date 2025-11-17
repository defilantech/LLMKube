#!/usr/bin/env bash
# GPU E2E Test Script for LLMKube
# Tests GPU model deployment, inference, and monitoring

set -e

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
TEST_NAMESPACE="default"
TEST_MODEL_NAME="e2e-test-gpu-model"
TEST_SERVICE_NAME="e2e-test-gpu-service"
MODEL_URL="https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf"

# Helper functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

cleanup() {
    log_info "Cleaning up test resources..."
    kubectl delete inferenceservice $TEST_SERVICE_NAME -n $TEST_NAMESPACE --ignore-not-found=true
    kubectl delete model $TEST_MODEL_NAME -n $TEST_NAMESPACE --ignore-not-found=true
    log_info "Cleanup complete"
}

# Trap to ensure cleanup on exit
trap cleanup EXIT

# Test 1: Deploy GPU Model
test_deploy_gpu_model() {
    log_info "Test 1: Deploying GPU model..."

    cat <<EOF | kubectl apply -f -
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: $TEST_MODEL_NAME
  namespace: $TEST_NAMESPACE
spec:
  source: $MODEL_URL
  format: gguf
  quantization: Q8_0
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      count: 1
      vendor: nvidia
      layers: -1
      memory: "8Gi"
  resources:
    cpu: "2"
    memory: "4Gi"
EOF

    log_info "✓ Model resource created"
}

# Test 2: Wait for Model to be Ready
test_model_ready() {
    log_info "Test 2: Waiting for model to be ready (max 5 minutes)..."

    kubectl wait --for=jsonpath='{.status.phase}'=Ready \
        model/$TEST_MODEL_NAME \
        -n $TEST_NAMESPACE \
        --timeout=300s || {
        log_error "Model failed to become ready"
        kubectl describe model $TEST_MODEL_NAME -n $TEST_NAMESPACE
        return 1
    }

    # Verify model details
    MODEL_SIZE=$(kubectl get model $TEST_MODEL_NAME -n $TEST_NAMESPACE -o jsonpath='{.status.size}')
    MODEL_PHASE=$(kubectl get model $TEST_MODEL_NAME -n $TEST_NAMESPACE -o jsonpath='{.status.phase}')

    log_info "✓ Model is ready"
    log_info "  Size: $MODEL_SIZE"
    log_info "  Phase: $MODEL_PHASE"
}

# Test 3: Deploy InferenceService
test_deploy_inference_service() {
    log_info "Test 3: Deploying InferenceService..."

    cat <<EOF | kubectl apply -f -
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: $TEST_SERVICE_NAME
  namespace: $TEST_NAMESPACE
spec:
  modelRef: $TEST_MODEL_NAME
  replicas: 1
  image: ghcr.io/ggerganov/llama.cpp:server-cuda
  endpoint:
    port: 8080
    path: /v1/chat/completions
    type: ClusterIP
  resources:
    gpu: 1
    gpuMemory: "8Gi"
    cpu: "2"
    memory: "4Gi"
EOF

    log_info "✓ InferenceService resource created"
}

# Test 4: Wait for InferenceService to be Ready
test_service_ready() {
    log_info "Test 4: Waiting for InferenceService to be ready (max 10 minutes)..."

    kubectl wait --for=jsonpath='{.status.phase}'=Ready \
        inferenceservice/$TEST_SERVICE_NAME \
        -n $TEST_NAMESPACE \
        --timeout=600s || {
        log_error "InferenceService failed to become ready"
        kubectl describe inferenceservice $TEST_SERVICE_NAME -n $TEST_NAMESPACE
        kubectl get pods -l app=$TEST_SERVICE_NAME -n $TEST_NAMESPACE
        return 1
    }

    READY_REPLICAS=$(kubectl get inferenceservice $TEST_SERVICE_NAME -n $TEST_NAMESPACE -o jsonpath='{.status.readyReplicas}')
    ENDPOINT=$(kubectl get inferenceservice $TEST_SERVICE_NAME -n $TEST_NAMESPACE -o jsonpath='{.status.endpoint}')

    log_info "✓ InferenceService is ready"
    log_info "  Ready Replicas: $READY_REPLICAS"
    log_info "  Endpoint: $ENDPOINT"
}

# Test 5: Verify GPU Scheduling
test_gpu_scheduling() {
    log_info "Test 5: Verifying GPU scheduling configuration..."

    POD_NAME=$(kubectl get pods -l app=$TEST_SERVICE_NAME -n $TEST_NAMESPACE -o jsonpath='{.items[0].metadata.name}')

    # Check GPU resource request
    GPU_REQUEST=$(kubectl get pod $POD_NAME -n $TEST_NAMESPACE -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}')
    if [ "$GPU_REQUEST" != "1" ]; then
        log_error "GPU resource request not found or incorrect: $GPU_REQUEST"
        return 1
    fi
    log_info "✓ GPU resource request: $GPU_REQUEST"

    # Check tolerations
    HAS_GPU_TOLERATION=$(kubectl get pod $POD_NAME -n $TEST_NAMESPACE -o jsonpath='{.spec.tolerations[?(@.key=="nvidia.com/gpu")].key}')
    if [ -z "$HAS_GPU_TOLERATION" ]; then
        log_error "GPU toleration not found"
        return 1
    fi
    log_info "✓ GPU toleration configured"

    # Check node selector
    NODE_SELECTOR=$(kubectl get pod $POD_NAME -n $TEST_NAMESPACE -o jsonpath='{.spec.nodeSelector.cloud\.google\.com/gke-nodepool}')
    if [ "$NODE_SELECTOR" != "gpu-pool" ]; then
        log_warn "Node selector: $NODE_SELECTOR (expected: gpu-pool)"
    else
        log_info "✓ Node selector: $NODE_SELECTOR"
    fi

    # Check GPU layer argument
    GPU_LAYERS=$(kubectl get pod $POD_NAME -n $TEST_NAMESPACE -o jsonpath='{.spec.containers[0].args}' | grep -o '\--n-gpu-layers [0-9]*' | awk '{print $2}')
    if [ -z "$GPU_LAYERS" ]; then
        log_error "GPU layers argument not found"
        return 1
    fi
    log_info "✓ GPU layers offloaded: $GPU_LAYERS"
}

# Test 6: Test Inference Endpoint
test_inference() {
    log_info "Test 6: Testing inference endpoint..."

    # Create a test pod to call the API
    kubectl run e2e-test-curl --rm -i --image=curlimages/curl --restart=Never -n $TEST_NAMESPACE -- \
        curl -s -X POST http://$TEST_SERVICE_NAME.$TEST_NAMESPACE.svc.cluster.local:8080/v1/chat/completions \
        -H "Content-Type: application/json" \
        -d '{"messages":[{"role":"user","content":"What is 2+2? Answer in one sentence."}],"max_tokens":50}' \
        > /tmp/inference_response.json

    # Verify response
    if ! grep -q "choices" /tmp/inference_response.json; then
        log_error "Invalid inference response"
        cat /tmp/inference_response.json
        return 1
    fi

    # Extract and display response
    RESPONSE=$(cat /tmp/inference_response.json | jq -r '.choices[0].message.content' 2>/dev/null || echo "Could not parse response")
    TOKENS=$(cat /tmp/inference_response.json | jq -r '.usage.total_tokens' 2>/dev/null || echo "unknown")

    log_info "✓ Inference successful"
    log_info "  Response: $RESPONSE"
    log_info "  Total tokens: $TOKENS"
}

# Test 7: Verify GPU Metrics
test_gpu_metrics() {
    log_info "Test 7: Verifying GPU metrics in Prometheus..."

    # Port-forward to Prometheus
    kubectl port-forward -n monitoring svc/kube-prometheus-stack-prometheus 9090:9090 > /dev/null 2>&1 &
    PF_PID=$!
    sleep 3

    # Query DCGM metrics
    METRICS=$(curl -s 'http://localhost:9090/api/v1/label/__name__/values' | jq -r '.data[]' | grep -i DCGM | head -5)

    kill $PF_PID 2>/dev/null || true

    if [ -z "$METRICS" ]; then
        log_error "No DCGM metrics found"
        return 1
    fi

    log_info "✓ GPU metrics available in Prometheus:"
    echo "$METRICS" | while read -r metric; do
        log_info "    $metric"
    done
}

# Test 8: Verify Alert Rules
test_alert_rules() {
    log_info "Test 8: Verifying alert rules are configured..."

    # Check PrometheusRule exists
    kubectl get prometheusrule llmkube-alerts -n monitoring > /dev/null 2>&1 || {
        log_error "PrometheusRule 'llmkube-alerts' not found"
        return 1
    }

    # Count alert rules
    ALERT_COUNT=$(kubectl get prometheusrule llmkube-alerts -n monitoring -o jsonpath='{.spec.groups[*].rules[*].alert}' | wc -w)

    log_info "✓ Alert rules configured: $ALERT_COUNT alerts"
}

# Main test execution
main() {
    log_info "Starting LLMKube GPU E2E Tests..."
    log_info "============================================"

    # Run tests
    test_deploy_gpu_model
    test_model_ready
    test_deploy_inference_service
    test_service_ready
    test_gpu_scheduling
    test_inference
    test_gpu_metrics
    test_alert_rules

    log_info "============================================"
    log_info "✓ All GPU E2E tests passed successfully!"
}

# Run main function
main

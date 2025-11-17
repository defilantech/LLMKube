#!/usr/bin/env bash
# Quick test script for GPU deployment
# Usage: ./test.sh

set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}LLMKube GPU Quickstart Test${NC}"
echo "================================"
echo ""

# Check prerequisites
echo -e "${YELLOW}Checking prerequisites...${NC}"

if ! command -v kubectl &> /dev/null; then
    echo -e "${RED}Error: kubectl not found${NC}"
    exit 1
fi

if ! kubectl get nodes &> /dev/null; then
    echo -e "${RED}Error: kubectl not configured or cluster unreachable${NC}"
    exit 1
fi

# Check for GPU nodes
GPU_NODES=$(kubectl get nodes -o json | jq -r '.items[] | select(.status.capacity."nvidia.com/gpu" != null) | .metadata.name' | wc -l)
if [ "$GPU_NODES" -eq 0 ]; then
    echo -e "${RED}Warning: No GPU nodes found in cluster${NC}"
    echo "This example requires GPU nodes with NVIDIA GPU Operator installed"
    exit 1
fi

echo -e "${GREEN}✓ Found $GPU_NODES GPU node(s)${NC}"
echo ""

# Deploy model
echo -e "${YELLOW}Deploying GPU model...${NC}"
kubectl apply -f model.yaml

# Wait for model
echo -e "${YELLOW}Waiting for model download (this may take 1-2 minutes for 3.2GB model)...${NC}"
kubectl wait --for=jsonpath='{.status.phase}'=Ready model/llama-3b-gpu --timeout=300s || {
    echo -e "${RED}Model failed to become ready${NC}"
    kubectl describe model llama-3b-gpu
    exit 1
}

MODEL_SIZE=$(kubectl get model llama-3b-gpu -o jsonpath='{.status.size}')
echo -e "${GREEN}✓ Model ready (size: $MODEL_SIZE)${NC}"
echo ""

# Deploy inference service
echo -e "${YELLOW}Deploying InferenceService...${NC}"
kubectl apply -f inferenceservice.yaml

# Wait for service
echo -e "${YELLOW}Waiting for service to be ready (this may take 1-2 minutes)...${NC}"
kubectl wait --for=jsonpath='{.status.phase}'=Ready inferenceservice/llama-3b-gpu-service --timeout=600s || {
    echo -e "${RED}InferenceService failed to become ready${NC}"
    kubectl describe inferenceservice llama-3b-gpu-service
    kubectl get pods -l app=llama-3b-gpu-service
    exit 1
}

READY_REPLICAS=$(kubectl get inferenceservice llama-3b-gpu-service -o jsonpath='{.status.readyReplicas}')
echo -e "${GREEN}✓ InferenceService ready (replicas: $READY_REPLICAS)${NC}"
echo ""

# Verify GPU scheduling
echo -e "${YELLOW}Verifying GPU configuration...${NC}"
POD_NAME=$(kubectl get pods -l app=llama-3b-gpu-service -o jsonpath='{.items[0].metadata.name}')

GPU_REQUEST=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}')
echo -e "${GREEN}✓ GPU resource request: $GPU_REQUEST${NC}"

GPU_LAYERS=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].args}' | grep -o '\--n-gpu-layers [0-9]*' | awk '{print $2}')
echo -e "${GREEN}✓ GPU layers to offload: $GPU_LAYERS${NC}"

NODE_NAME=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.nodeName}')
echo -e "${GREEN}✓ Scheduled on node: $NODE_NAME${NC}"
echo ""

# Test inference
echo -e "${YELLOW}Testing inference endpoint...${NC}"
echo "Starting port-forward in background..."

kubectl port-forward svc/llama-3b-gpu-service 8080:8080 > /dev/null 2>&1 &
PF_PID=$!
sleep 3

# Simple inference test
echo -e "${YELLOW}Sending test request...${NC}"
RESPONSE=$(curl -s http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "What is 2+2? Answer in one word."}],
    "max_tokens": 10
  }')

kill $PF_PID 2>/dev/null || true

if echo "$RESPONSE" | jq -e '.choices[0].message.content' > /dev/null 2>&1; then
    CONTENT=$(echo "$RESPONSE" | jq -r '.choices[0].message.content')
    TOKENS=$(echo "$RESPONSE" | jq -r '.usage.total_tokens')
    echo -e "${GREEN}✓ Inference successful${NC}"
    echo "  Response: $CONTENT"
    echo "  Total tokens: $TOKENS"
else
    echo -e "${RED}Error: Invalid response${NC}"
    echo "$RESPONSE"
    exit 1
fi

echo ""
echo -e "${GREEN}================================${NC}"
echo -e "${GREEN}All tests passed! 🎉${NC}"
echo -e "${GREEN}================================${NC}"
echo ""
echo "Your GPU-accelerated LLM is ready!"
echo ""
echo "To access the API:"
echo "  kubectl port-forward svc/llama-3b-gpu-service 8080:8080"
echo ""
echo "To test inference:"
echo "  curl http://localhost:8080/v1/chat/completions \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"messages\":[{\"role\":\"user\",\"content\":\"Hello!\"}],\"max_tokens\":50}'"
echo ""
echo "To check GPU metrics:"
echo "  kubectl logs $POD_NAME | grep -i gpu"
echo ""
echo "To cleanup:"
echo "  kubectl delete -f inferenceservice.yaml"
echo "  kubectl delete -f model.yaml"
echo ""

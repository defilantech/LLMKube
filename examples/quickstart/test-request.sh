#!/bin/bash
# Test script for TinyLlama inference service
# Usage: ./test-request.sh [service-url]

SERVICE_URL="${1:-http://localhost:8080}"

echo "Testing LLMKube inference service at: $SERVICE_URL"
echo "---"

curl -X POST "$SERVICE_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "tinyllama",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "What is Kubernetes? Answer in one sentence."}
    ],
    "max_tokens": 50,
    "temperature": 0.7
  }' | jq .

echo ""
echo "---"
echo "Test complete!"

# Metal Quickstart - Local LLM Inference on Apple Silicon

This guide shows you how to deploy GPU-accelerated LLM inference on your Mac using **Metal** (Apple's GPU framework) with Kubernetes orchestration.

**Performance:** Get Ollama-level speeds (60-120 tok/s) with Kubernetes-native deployment! 🚀

## Prerequisites

### Hardware
- **macOS** with Apple Silicon (M1/M2/M3/M4) or Intel Mac with Metal 2+
- Recommended: M-series Mac with 16GB+ RAM

### Software
1. **Minikube** with Docker driver
   ```bash
   brew install minikube
   minikube start --driver=docker
   ```

2. **llama.cpp** with Metal support
   ```bash
   brew install llama.cpp
   ```

3. **LLMKube CLI**
   ```bash
   # Download latest release
   curl -L https://github.com/defilantech/llmkube/releases/latest/download/llmkube_$(uname -s)_$(uname -m).tar.gz | tar xz
   sudo mv llmkube /usr/local/bin/
   ```

4. **LLMKube Operator**
   ```bash
   kubectl apply -f https://github.com/defilantech/llmkube/releases/latest/download/install.yaml
   ```

5. **Metal Agent**
   ```bash
   # From llmkube repository
   make install-metal-agent

   # Or download pre-built binary
   curl -L https://github.com/defilantech/llmkube/releases/latest/download/llmkube-metal-agent_$(uname -s)_$(uname -m).tar.gz | tar xz
   sudo mv llmkube-metal-agent /usr/local/bin/

   # Install and start the service
   make install-metal-agent
   ```

## Verify Setup

```bash
# 1. Check Metal support
system_profiler SPDisplaysDataType | grep "Metal"
# Should show: Metal Support: Metal 4 (or Metal 3/2)

# 2. Check llama-server is installed
which llama-server
# Should show: /usr/local/bin/llama-server

# 3. Check Metal agent is running
launchctl list | grep llmkube
# Should show: com.llmkube.metal-agent

# 4. Check minikube is running
minikube status
# Should show: host: Running, kubelet: Running

# 5. Check kubectl works
kubectl get nodes
# Should show your minikube node
```

## Quick Start

### Option 1: Deploy from Catalog (Recommended)

```bash
# Deploy Llama 3.1 8B with Metal acceleration
llmkube deploy llama-3.1-8b --gpu

# The CLI will auto-detect Metal and use it!
# Output:
# ℹ️  Auto-detected accelerator: metal (Apple Silicon GPU)
# ℹ️  Metal acceleration: Using native llama-server (not containerized)
# 📚 Using catalog model: Llama 3.1 8B Instruct
# 🚀 Deploying LLM inference service
# ...
```

### Option 2: Explicit Metal Flag

```bash
# Explicitly specify Metal accelerator
llmkube deploy llama-3.1-8b --accelerator metal

# Or with custom settings
llmkube deploy qwen-2.5-coder-7b \
  --accelerator metal \
  --gpu-layers 33 \
  --memory 8Gi
```

### Option 3: Custom Model

```bash
llmkube deploy my-custom-model \
  --accelerator metal \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf \
  --quantization Q8_0 \
  --gpu-layers 28
```

## Monitor the Deployment

```bash
# Watch the deployment
kubectl get inferenceservices -w

# Check Metal agent logs
tail -f /tmp/llmkube-metal-agent.log

# Monitor GPU usage
sudo powermetrics --samplers gpu_power -i 1000
```

## Test the Endpoint

Once deployed, test your inference service:

```bash
# Port forward the service
kubectl port-forward svc/llama-3.1-8b 8080:8080

# Send a test request (in another terminal)
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [
      {"role": "user", "content": "What is 2+2?"}
    ]
  }'
```

## Performance Expectations

On M4 Max (32 GPU cores):

| Model | Generation Speed | Prompt Processing | VRAM Usage |
|-------|-----------------|-------------------|------------|
| **Llama 3.2 3B** | 80-120 tok/s | 1000+ tok/s | 2-3 GB |
| **Llama 3.1 8B** | 40-60 tok/s | 500-800 tok/s | 5-8 GB |
| **Mistral 7B** | 45-65 tok/s | 600-900 tok/s | 5-7 GB |
| **Qwen Coder 7B** | 40-55 tok/s | 500-750 tok/s | 5-8 GB |

## How It Works

```
┌─────────────────────────────────────────────────┐
│              Your Mac (macOS)                    │
│                                                  │
│  ┌──────────────────────────────────────────┐   │
│  │   Minikube (Kubernetes in VM)            │   │
│  │   ✅ Creates InferenceService CRD        │   │
│  │   ✅ Service points to host              │   │
│  └──────────────────────────────────────────┘   │
│                     ↕                            │
│  ┌──────────────────────────────────────────┐   │
│  │   Metal Agent (Native Process)           │   │
│  │   ✅ Watches K8s for InferenceService    │   │
│  │   ✅ Downloads model from HuggingFace    │   │
│  │   ✅ Spawns llama-server with Metal      │   │
│  └──────────────────────────────────────────┘   │
│                     ↕                            │
│  ┌──────────────────────────────────────────┐   │
│  │   llama-server (Metal Accelerated) 🚀   │   │
│  │   ✅ Runs on localhost:8080+             │   │
│  │   ✅ Direct Metal GPU access             │   │
│  │   ✅ OpenAI-compatible API               │   │
│  └──────────────────────────────────────────┘   │
└─────────────────────────────────────────────────┘
```

## Troubleshooting

### Metal agent not starting

```bash
# Check logs
cat /tmp/llmkube-metal-agent.log

# Verify llama-server is installed
llama-server --version

# Restart the agent
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

### Model download slow/failing

```bash
# Check disk space
df -h

# Check model store
ls -lh /tmp/llmkube-models/

# Manually download model
curl -L https://huggingface.co/bartowski/Meta-Llama-3.1-8B-Instruct-GGUF/resolve/main/Meta-Llama-3.1-8B-Instruct-Q5_K_M.gguf \
  -o /tmp/llmkube-models/llama-3.1-8b/model.gguf
```

### Service not accessible

```bash
# Check service exists
kubectl get svc llama-3.1-8b

# Check endpoints
kubectl get endpoints llama-3.1-8b

# Check if llama-server is running
ps aux | grep llama-server

# Check Metal agent can reach K8s
kubectl get inferenceservices
```

### Poor performance

```bash
# Verify Metal is being used
llama-server --version  # Should show Metal support

# Check GPU utilization
sudo powermetrics --samplers gpu_power -i 1000

# Verify all layers offloaded to GPU
# Check Metal agent logs for "n-gpu-layers"
grep "n-gpu-layers" /tmp/llmkube-metal-agent.log
```

## Cleanup

```bash
# Delete the inference service
llmkube delete llama-3.1-8b

# Stop Metal agent
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist

# Or completely uninstall
make uninstall-metal-agent
```

## Next Steps

- **Scale up**: Try larger models (Mixtral 8x7B, Llama 70B)
- **Production**: Deploy multiple replicas for high availability
- **Integration**: Connect to your applications using OpenAI SDK
- **Monitoring**: Set up Prometheus + Grafana dashboards

## Example Applications

### Python Client

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="not-needed"
)

response = client.chat.completions.create(
    model="llama-3.1-8b",
    messages=[
        {"role": "user", "content": "Explain quantum computing"}
    ]
)

print(response.choices[0].message.content)
```

### cURL Example

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-3.1-8b",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "Write a haiku about Kubernetes"}
    ],
    "temperature": 0.7,
    "max_tokens": 100
  }'
```

## Support

- **Documentation**: https://github.com/defilantech/llmkube#metal-support
- **Issues**: https://github.com/defilantech/llmkube/issues
- **Discussions**: https://github.com/defilantech/llmkube/discussions

## Performance Tips

1. **Use Q5_K_M quantization** - Best balance of quality and speed
2. **Offload all layers** - Set `--gpu-layers 99` for maximum Metal usage
3. **Close other apps** - Free up GPU resources for better performance
4. **Monitor temperature** - Keep your Mac cool for sustained performance
5. **Use catalog models** - Pre-optimized settings for your hardware

---

**Congratulations!** 🎉 You're now running Kubernetes-native LLM inference with Metal acceleration on your Mac!

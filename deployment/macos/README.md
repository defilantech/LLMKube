# LLMKube Metal Agent for macOS

This directory contains the macOS launchd configuration for the LLMKube Metal Agent, which enables Metal GPU acceleration for local Kubernetes LLM deployments.

## Prerequisites

1. **macOS with Apple Silicon** (M1/M2/M3/M4) or Intel Mac with Metal 2+ support
2. **Minikube** running with Docker driver
3. **llama.cpp** with Metal support:
   ```bash
   brew install llama.cpp
   ```
4. **LLMKube operator** installed in minikube:
   ```bash
   kubectl apply -f https://github.com/defilantech/llmkube/releases/latest/download/install.yaml
   ```

## Installation

### Option 1: Using Makefile (Recommended)

```bash
# Build and install Metal agent
make install-metal-agent
```

This will:
- Build the Metal agent binary
- Install to `/usr/local/bin/llmkube-metal-agent`
- Install launchd service
- Start the service automatically

### Option 2: Manual Installation

```bash
# Build the agent
make build-metal-agent

# Copy to /usr/local/bin
sudo cp bin/llmkube-metal-agent /usr/local/bin/

# Install launchd plist
mkdir -p ~/Library/LaunchAgents
cp deployment/macos/com.llmkube.metal-agent.plist ~/Library/LaunchAgents/

# Load the service
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

## Usage

Once installed, the Metal agent runs automatically in the background and watches for InferenceService resources in your Kubernetes cluster.

### Deploy a Model with Metal Acceleration

```bash
# Deploy from catalog
llmkube deploy llama-3.1-8b --accelerator metal

# Or deploy custom model
llmkube deploy my-model --accelerator metal \
  --source https://huggingface.co/.../model.gguf
```

### Check Agent Status

```bash
# Check if agent is running
launchctl list | grep llmkube

# View agent logs
tail -f /tmp/llmkube-metal-agent.log

# Check running processes
ps aux | grep llmkube-metal-agent
```

### Verify Metal Acceleration

```bash
# Check Metal support
system_profiler SPDisplaysDataType | grep Metal

# Monitor GPU usage while inference is running
sudo powermetrics --samplers gpu_power -i 1000
```

## Configuration

The launchd plist can be customized by editing `com.llmkube.metal-agent.plist`:

```xml
<key>ProgramArguments</key>
<array>
    <string>/usr/local/bin/llmkube-metal-agent</string>
    <string>--namespace</string>
    <string>default</string>              <!-- Kubernetes namespace to watch -->
    <string>--model-store</string>
    <string>/tmp/llmkube-models</string>  <!-- Where to store downloaded models -->
    <string>--llama-server</string>
    <string>/usr/local/bin/llama-server</string>  <!-- Path to llama-server binary -->
    <string>--port</string>
    <string>9090</string>                 <!-- Agent metrics port -->
</array>
```

After editing, reload the service:
```bash
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

## Troubleshooting

### Agent won't start

```bash
# Check logs
cat /tmp/llmkube-metal-agent.log

# Verify llama-server is installed
which llama-server

# Verify Metal support
llmkube-metal-agent --version
```

### Metal not detected

```bash
# Verify GPU info
system_profiler SPDisplaysDataType

# Check for Metal support
system_profiler SPDisplaysDataType | grep "Metal"
```

### Can't connect to Kubernetes

```bash
# Verify minikube is running
minikube status

# Verify kubectl works
kubectl get nodes

# Check kubeconfig
echo $KUBECONFIG
```

## Uninstallation

```bash
# Using Makefile
make uninstall-metal-agent

# Or manually
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
sudo rm /usr/local/bin/llmkube-metal-agent
rm ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

## How It Works

1. **Metal Agent** runs as a native macOS process (not in Kubernetes)
2. **Watches** for InferenceService resources in Kubernetes
3. **Downloads** models from HuggingFace when needed
4. **Spawns** llama-server processes with Metal acceleration
5. **Registers** service endpoints back to Kubernetes
6. **Pods** access the Metal-accelerated inference via Service endpoints

```
┌─────────────────────────────────────────────────┐
│              macOS (Your Mac)                    │
│                                                  │
│  ┌──────────────────────────────────────────┐   │
│  │   Minikube (Kubernetes in VM)            │   │
│  │   - Creates InferenceService CRD         │   │
│  │   - Service points to host               │   │
│  └──────────────────────────────────────────┘   │
│                     ↓                            │
│  ┌──────────────────────────────────────────┐   │
│  │   Metal Agent (Native Process)           │   │
│  │   - Watches K8s for InferenceService     │   │
│  │   - Spawns llama-server with Metal       │   │
│  └──────────────────────────────────────────┘   │
│                     ↓                            │
│  ┌──────────────────────────────────────────┐   │
│  │   llama-server (Metal Accelerated)       │   │
│  │   - Runs on localhost:8080+              │   │
│  │   - Direct Metal GPU access ✅           │   │
│  └──────────────────────────────────────────┘   │
└─────────────────────────────────────────────────┘
```

## Performance

Expected performance on M4 Max (32 GPU cores):
- **Llama 3.2 3B**: 80-120 tok/s (generation)
- **Llama 3.1 8B**: 40-60 tok/s (generation)
- **Mistral 7B**: 45-65 tok/s (generation)

Performance comparable to Ollama, but with Kubernetes orchestration!

## Security

- Agent runs as your user (not root)
- Models stored in `/tmp/llmkube-models` (configurable)
- Processes bind to localhost only
- Service endpoints use ClusterIP (not exposed externally)

## Support

- GitHub Issues: https://github.com/defilantech/llmkube/issues
- Documentation: https://github.com/defilantech/llmkube#metal-support

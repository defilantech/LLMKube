# LLMKube v0.3.0 Release Notes

**Release Date**: November 23, 2025
**Status**: Metal GPU Support + Model Catalog
**Codename**: "Metal Launch" 🍎

## Overview

LLMKube v0.3.0 introduces **Metal GPU support for Apple Silicon** and completes the **Model Catalog** feature. This release enables developers to test LLM workloads locally on their Mac with native GPU acceleration, then deploy to cloud with CUDA using the same Kubernetes configurations.

**TL;DR**: Run production-grade LLM inference on your MacBook (M1/M2/M3/M4) with 60-120 tok/s performance, deploy catalog models in one command, and seamlessly transition from local development to cloud production.

## 🚀 What's New

### Metal GPU Support for macOS (Apple Silicon)

#### 🍎 Native Metal GPU Acceleration

**Full support for Apple Silicon GPUs** (M1/M2/M3/M4):
- **60-120 tok/s generation** on M4 Max (Llama 3.1 8B: 40-60 tok/s, Llama 3.2 3B: 80-120 tok/s)
- **Native llama-server processes** with Metal GPU offloading
- **Hybrid architecture**: Kubernetes orchestration + native Metal performance
- **Same performance as Ollama** with Kubernetes-native benefits

#### 🔧 Metal Agent Daemon

**Background service for macOS** that manages llama-server processes:
- Watches InferenceService CRDs and spawns native processes
- Automatic Service and Endpoints creation for cluster integration
- Health checking and process lifecycle management
- Configurable via LaunchAgent (`deployment/macos/com.llmkube.metal-agent.plist`)

#### 🎯 One-Command Metal Deployment

```bash
# Install llama.cpp (one time)
brew install llama.cpp

# Deploy with Metal GPU acceleration
llmkube deploy llama-3.1-8b --accelerator metal

# Deploy with custom GPU layer configuration
llmkube deploy llama-3.2-3b --accelerator metal --gpu-layers 99
```

**Automatic platform detection**: The CLI automatically detects Metal availability and configures GPU offloading.

#### 🌉 Hybrid Architecture

**Kubernetes orchestration + native Metal processes**:

```
┌─────────────────────────────────────────────┐
│ Kubernetes (minikube)                       │
│  ┌──────────────────────────────────────┐  │
│  │ InferenceService CRD                 │  │
│  │ - Name: llama-3.1-8b                 │  │
│  │ - Accelerator: metal                 │  │
│  └──────────────────────────────────────┘  │
│                  │                          │
│                  ▼                          │
│  ┌──────────────────────────────────────┐  │
│  │ Service: llama-3.1-8b                │  │
│  │ - ClusterIP: 10.96.x.x:8080          │  │
│  └──────────────────────────────────────┘  │
│                  │                          │
│                  ▼                          │
│  ┌──────────────────────────────────────┐  │
│  │ Endpoints (manual)                   │  │
│  │ - IP: 192.168.65.254 (host)          │  │
│  │ - Port: 8080                         │  │
│  └──────────────────────────────────────┘  │
└─────────────────────────────────────────────┘
                  │
                  │ (bridges to host)
                  ▼
┌─────────────────────────────────────────────┐
│ macOS Host                                  │
│  ┌──────────────────────────────────────┐  │
│  │ Metal Agent (background daemon)      │  │
│  │ - Watches CRDs                       │  │
│  │ - Manages native processes          │  │
│  └──────────────────────────────────────┘  │
│                  │                          │
│                  ▼                          │
│  ┌──────────────────────────────────────┐  │
│  │ llama-server (native process)        │  │
│  │ - Metal GPU acceleration             │  │
│  │ - Listening on localhost:8080        │  │
│  │ - All layers offloaded to GPU        │  │
│  └──────────────────────────────────────┘  │
└─────────────────────────────────────────────┘
```

#### 📊 Real Performance Numbers

Tested on M4 Max (16-core CPU, 40-core GPU, 128GB RAM):

| Model | Model Size | Quantization | Prompt Processing | Generation | VRAM Usage |
|-------|-----------|--------------|------------------|-----------|-----------|
| Llama 3.1 8B | 8.0B | Q5_K_M | ~400 tok/s | 40-60 tok/s | 5-6GB |
| Llama 3.2 3B | 3.2B | Q5_K_M | ~800 tok/s | 80-120 tok/s | 2-3GB |
| Mistral 7B | 7.2B | Q5_K_M | ~450 tok/s | 45-65 tok/s | 5-6GB |

**Key Benefits**:
- Same speeds as native Ollama
- Full Kubernetes orchestration (services, scaling, monitoring)
- OpenAI-compatible API endpoints
- Unified workflow: test locally → deploy to cloud

### Multi-Platform CLI Builds

#### 📦 GoReleaser Integration

**Automated multi-platform binary releases**:
- **macOS**: Intel (amd64) + Apple Silicon (arm64)
- **Linux**: amd64 + arm64
- **Windows**: amd64

**Separate Metal Agent Binary** (macOS only):
- `llmkube-metal-agent` for macOS (Intel + Apple Silicon)
- Included in separate archive: `LLMKube-metal-agent_0.3.0_darwin_*.tar.gz`
- Configured as LaunchAgent for background operation

#### 🍺 Homebrew Formula

**Easy installation on macOS**:
```bash
# Add the tap
brew tap defilantech/tap

# Install LLMKube CLI
brew install llmkube

# Verify installation
llmkube version
```

### Enhanced Developer Experience

#### 🔄 Unified Workflow

**Same Kubernetes CRDs work across platforms**:

```bash
# 1. Local development on Mac with Metal
llmkube deploy llama-3.1-8b --accelerator metal

# 2. Test your application locally
kubectl port-forward svc/llama-3.1-8b 8080:8080
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"Hello!"}]}'

# 3. Deploy to cloud with CUDA (same config!)
llmkube deploy llama-3.1-8b --gpu

# Same InferenceService spec works on both platforms
```

#### 📚 Comprehensive Documentation

**New documentation added**:
- **Metal Quick Start Guide**: `examples/metal-quickstart/README.md`
  - Architecture diagrams and explanations
  - Step-by-step setup instructions
  - Troubleshooting and performance tuning
- **macOS Deployment Guide**: `deployment/macos/README.md`
  - Production deployment instructions
  - LaunchAgent configuration
  - System requirements and prerequisites

### Model Catalog Improvements

The **Model Catalog** introduced in v0.2.2 continues to be available:

```bash
# Browse 10+ pre-configured models
llmkube catalog list

# Deploy any model in one command
llmkube deploy llama-3.1-8b --accelerator metal  # macOS
llmkube deploy llama-3.1-8b --gpu                # Cloud (CUDA)
llmkube deploy llama-3.2-3b --cpu 2 --memory 4Gi # CPU-only
```

## 🛠️ Technical Details

### Architecture Changes

#### Service Registry

**Manual Endpoints management** to bridge Kubernetes services to native host processes:
- Creates Service resources with no selectors
- Manually creates Endpoints pointing to `host.minikube.internal` (192.168.65.254)
- Enables Kubernetes service discovery for native processes
- Uses deprecated Endpoints API (still functional and appropriate for this use case)

#### Platform Detection

**Automatic Metal availability detection**:
- Checks for Metal framework availability
- Detects Apple Silicon vs Intel Macs
- Verifies llama-server binary with Metal support
- Provides helpful error messages if prerequisites missing

#### Process Management

**Native process lifecycle management**:
- Downloads models to `/tmp/llmkube-models/` (configurable)
- Spawns llama-server with Metal-specific environment variables
- Health checks via `/health` endpoint
- Graceful shutdown (SIGTERM with 10s timeout)
- Port allocation (starting from 8080)

### Breaking Changes

**No Breaking Changes** - v0.3.0 is fully backward compatible with v0.2.x.

All existing deployments, configurations, and workflows continue to work unchanged.

### Deprecation Notices

- The Kubernetes Endpoints API is deprecated in v1.33+ in favor of EndpointSlice
  - LLMKube continues to use Endpoints API for manual endpoint management
  - This is intentional and appropriate for the hybrid architecture
  - Will migrate to EndpointSlice in a future release when Kubernetes support matures

## 📦 Installation & Upgrade

### New Installation

#### macOS (Homebrew)
```bash
# Add the tap
brew tap defilantech/tap

# Install CLI
brew install llmkube

# Install llama.cpp for Metal support
brew install llama.cpp

# Verify installation
llmkube version
```

#### macOS (Manual)
```bash
# AMD64 (Intel)
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.0/LLMKube_0.3.0_darwin_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/

# ARM64 (Apple Silicon)
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.0/LLMKube_0.3.0_darwin_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

#### Linux
```bash
# AMD64
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.0/LLMKube_0.3.0_linux_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/

# ARM64
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.0/LLMKube_0.3.0_linux_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

#### Windows
Download the `.zip` file for your architecture and add `llmkube.exe` to your PATH.

### Upgrading from v0.2.x

**Simple CLI upgrade**:
```bash
# Homebrew (macOS)
brew upgrade llmkube

# Or download new binary manually
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.0/LLMKube_0.3.0_[OS]_[ARCH].tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**No cluster changes needed** - the controller version remains compatible.

### Metal Agent Setup (macOS only)

For Metal GPU support, install the Metal agent as a LaunchAgent:

```bash
# Download Metal agent
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.0/LLMKube-metal-agent_0.3.0_darwin_[ARCH].tar.gz | tar xz
sudo mv llmkube-metal-agent /usr/local/bin/

# Install LaunchAgent
curl -L https://raw.githubusercontent.com/defilantech/LLMKube/main/deployment/macos/com.llmkube.metal-agent.plist -o ~/Library/LaunchAgents/com.llmkube.metal-agent.plist

# Start the agent
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist

# Verify it's running
launchctl list | grep llmkube
```

See `deployment/macos/README.md` for detailed instructions.

## 🐛 Bug Fixes

- Fixed nil pointer dereference when GPU spec is nil (pkg/agent/agent.go:145)
- Suppressed Endpoints API deprecation warnings with appropriate nolint directives
- Resolved linter issues in Metal agent code
- Fixed production stability issues in Metal agent

## 📊 What's Next

### Sprint 3 (Next)

**Multi-GPU single-node support**:
- Test 13B models on 2x L4 GPUs
- Implement multi-GPU layer distribution
- Update InferenceService CRD for multi-GPU configuration
- Performance benchmarks for multi-GPU vs single GPU

### Future Roadmap

**Phase 1: Core MVP** (Sprints 3-7):
- Sprint 3: Multi-GPU single-node support
- Sprints 4-5: Production hardening (health checks, auto-scaling, security)
- Sprints 6-7: GPU shard scheduler (multi-node)

**Phase 2: Production Hardening** (Sprints 8-10):
- SLO controller and auto-scaling
- Compiler POC for natural language deployment
- Edge deployment pilot

See [ROADMAP.md](ROADMAP.md) for complete roadmap.

## 🙏 Acknowledgments

- **llama.cpp community** for Metal GPU support
- **bartowski** for high-quality GGUF model conversions
- **GoReleaser team** for excellent multi-platform build automation
- **GitHub Actions** for reliable CI/CD

## 📝 Full Changelog

See [CHANGELOG.md](CHANGELOG.md) for the complete list of changes.

**Previous Release**: [v0.2.2](https://github.com/defilantech/LLMKube/releases/tag/v0.2.2)

## 🔗 Resources

- **Documentation**: [README.md](README.md)
- **Metal Quick Start**: [examples/metal-quickstart/README.md](examples/metal-quickstart/README.md)
- **GPU Quick Start**: [examples/gpu-quickstart/README.md](examples/gpu-quickstart/README.md)
- **Model Catalog**: Run `llmkube catalog list` to see all available models
- **Roadmap**: [ROADMAP.md](ROADMAP.md)
- **Contributing**: [CONTRIBUTING.md](CONTRIBUTING.md)

## 💬 Community & Support

- **GitHub Issues**: [Report bugs or request features](https://github.com/defilantech/LLMKube/issues)
- **Discussions**: [Ask questions and share ideas](https://github.com/defilantech/LLMKube/discussions)

---

**Version**: v0.3.0
**Release Date**: November 23, 2025
**License**: Apache 2.0

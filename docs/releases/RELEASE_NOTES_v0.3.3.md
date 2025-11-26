# LLMKube v0.3.3 Release Notes

**Release Date**: November 24, 2025
**Status**: Critical Bug Fix
**Codename**: "DNS Compliant" 🔧

## Overview

LLMKube v0.3.3 is a patch release that fixes a critical bug preventing deployment of catalog models with dots in their names (e.g., `llama-3.1-8b`, `qwen-2.5-coder-7b`). This issue affected the Metal accelerator quickstart guide and any deployment using versioned model names.

**TL;DR**: Models with dots in their names (like `llama-3.1-8b`) now deploy successfully. Service names are automatically sanitized to comply with Kubernetes DNS-1035 requirements.

## 🐛 Bug Fixes

### Service Name DNS-1035 Compliance (Issue #44)

**Fixed: Controller fails to create Service for models with dots in names**

#### Problem
When deploying models with dots in their names (common in version numbers like `llama-3.1-8b`), the controller would fail to create the Kubernetes Service because dots are not allowed in DNS-1035 labels.

**Example error seen:**
```
Service "llama-3.1-8b" is invalid: metadata.name: Invalid value: "llama-3.1-8b":
a DNS-1035 label must consist of lower case alphanumeric characters or '-',
start with an alphabetic character, and end with an alphanumeric character
```

**Impact:**
- Metal quickstart guide failed at deployment step
- Any catalog model with version numbers failed to deploy
- Users received confusing error messages about DNS compliance

#### Root Cause
The controller's `constructService` function was using the InferenceService name directly without sanitization. While the Metal agent code (added in v0.3.0) already sanitized Service names, the controller code was not updated with the same fix. Since the controller runs first, it would fail before the Metal agent could execute.

#### Solution
Added DNS name sanitization to both the controller and CLI:

**Controller (`internal/controller/inferenceservice_controller.go`):**
- Added `sanitizeDNSName()` helper function that replaces dots with dashes
- Applied sanitization when creating Service resources
- Service name: `llama-3.1-8b` → `llama-3-1-8b`

**CLI (`pkg/cli/deploy.go`):**
- Added matching `sanitizeServiceName()` helper function
- Fixed port-forward command in deployment output to show correct sanitized name
- Users now see the correct command that will actually work

**Example output:**
```bash
✅ Deployment ready!
═══════════════════════════════════════════════
Model:       llama-3.1-8b
Endpoint:    http://llama-3-1-8b.default.svc.cluster.local:8080/v1/chat/completions
Replicas:    1/1
═══════════════════════════════════════════════

🧪 To test the inference endpoint:

  # Port forward the service
  kubectl port-forward -n default svc/llama-3-1-8b 8080:8080

  # Send a test request
  curl http://localhost:8080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"messages":[{"role":"user","content":"What is 2+2?"}]}'
```

#### Impact
- ✅ Metal quickstart now works end-to-end without errors
- ✅ All catalog models deploy successfully regardless of naming
- ✅ CLI output shows correct Service names users can actually use
- ✅ Better consistency between controller and Metal agent behavior

## 🛠️ Technical Details

### Files Changed
- `internal/controller/inferenceservice_controller.go` - Added `sanitizeDNSName()` and applied to Service creation
- `pkg/cli/deploy.go` - Added `sanitizeServiceName()` and updated output formatting

### Sanitization Logic
```go
// sanitizeDNSName converts a string to be DNS-1035 compliant
// DNS-1035 requires: lowercase alphanumeric or '-', start with alphabetic, end with alphanumeric
func sanitizeDNSName(name string) string {
    // Replace dots with dashes
    return strings.ReplaceAll(name, ".", "-")
}
```

### Testing
- ✅ Controller compiles and all tests pass
- ✅ Deployed `llama-3.1-8b` successfully with Metal accelerator
- ✅ Service created with sanitized name: `llama-3-1-8b`
- ✅ CLI output verified to show correct commands
- ✅ End-to-end Metal quickstart guide tested successfully

## 📦 Installation & Upgrade

### Upgrading from v0.3.2 or earlier

#### macOS (Homebrew)
```bash
brew upgrade llmkube
```

#### Manual Installation

**macOS (Apple Silicon):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.3/LLMKube_0.3.3_darwin_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**macOS (Intel):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.3/LLMKube_0.3.3_darwin_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Linux (amd64):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.3/LLMKube_0.3.3_linux_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Linux (arm64):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.3/LLMKube_0.3.3_linux_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

### Controller Upgrade Required

This release requires updating the controller image as well as the CLI:

**Option 1: Helm (Recommended)**
```bash
helm upgrade llmkube https://github.com/defilantech/LLMKube/releases/download/v0.3.3/llmkube-0.3.3.tgz \
  --namespace llmkube-system
```

**Option 2: Kustomize**
```bash
kubectl apply -k https://github.com/defilantech/LLMKube/config/default?ref=v0.3.3
```

**Option 3: Development (local minikube)**
```bash
eval $(minikube docker-env)
make docker-build deploy
kubectl delete pod -n llmkube-system -l control-plane=controller-manager
```

## 🧪 Verification

After upgrading, verify the fix:

### Test Deployment with Dotted Name
```bash
# Deploy a model with dots in the name
llmkube deploy llama-3.1-8b --accelerator metal

# Should complete successfully without DNS errors
# ✅ Deployment ready!

# Verify Service was created with sanitized name
kubectl get service llama-3-1-8b

# Test the service (note the sanitized name)
kubectl port-forward svc/llama-3-1-8b 8080:8080
```

### Verify Correct CLI Output
The CLI should now show the correct sanitized service name in the port-forward command:
```bash
kubectl port-forward -n default svc/llama-3-1-8b 8080:8080
```

Not the broken command with dots:
```bash
kubectl port-forward -n default svc/llama-3.1-8b 8080:8080  # ❌ This won't work
```

## 🔄 Upgrade Impact

**Minimal Breaking Changes** - v0.3.3 is mostly backward compatible with v0.3.2.

- Existing deployments without dots in names continue to work unchanged
- **Note:** If you previously deployed models with dots in their names and they failed, you'll need to redeploy them after upgrading. The old failed resources can be cleaned up:
  ```bash
  kubectl delete inferenceservice llama-3.1-8b
  kubectl delete model llama-3.1-8b
  # Then redeploy with the upgraded controller
  llmkube deploy llama-3.1-8b --accelerator metal
  ```

## 📝 Full Changelog

### Bug Fixes
- Fixed controller Service creation for model names with dots (#44)
- Service names are now properly sanitized to replace dots with dashes
- Fixed CLI output to display correct sanitized Service names
- Aligned controller behavior with Metal agent sanitization logic

### Improvements
- Better consistency between controller and Metal agent
- Clearer CLI output with working commands
- All catalog models now deploy successfully

## 📊 What's Next

### v0.3.4 (Planned)
- Additional stability improvements
- Enhanced error messages and troubleshooting
- Documentation updates

### v0.4.0 (Future)
- Multi-GPU single-node support
- Enhanced monitoring and observability
- Production hardening features

See [ROADMAP.md](ROADMAP.md) for complete roadmap.

## 🔗 Resources

- **Issue #44**: [Controller fails to create Service for models with dots in names](https://github.com/defilantech/LLMKube/issues/44)
- **Metal Quick Start**: [examples/metal-quickstart/README.md](examples/metal-quickstart/README.md)
- **Documentation**: [README.md](README.md)
- **Roadmap**: [ROADMAP.md](ROADMAP.md)

## 💬 Community & Support

- **GitHub Issues**: [Report bugs or request features](https://github.com/defilantech/LLMKube/issues)
- **Discussions**: [Ask questions and share ideas](https://github.com/defilantech/LLMKube/discussions)

---

**Version**: v0.3.3
**Release Date**: November 24, 2025
**License**: Apache 2.0

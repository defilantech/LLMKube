# LLMKube v0.3.2 Release Notes

**Release Date**: November 24, 2025
**Status**: Bug Fixes & Quality of Life Improvements
**Codename**: "Metal Polish" 🔧

## Overview

LLMKube v0.3.2 is a patch release that fixes a critical bug affecting Metal deployments on resource-constrained clusters and adds automatic version update notifications to improve user experience.

**TL;DR**: Metal deployments now work correctly on minikube without memory errors, and the CLI will notify you when updates are available.

## 🐛 Bug Fixes

### Metal Deployment Conflict (Issue #41)

**Fixed: Controller creating unnecessary containerized pods for Metal accelerator**

#### Problem
When deploying with `--accelerator metal`, the InferenceService controller was creating both:
1. A containerized Kubernetes Deployment (which failed due to resource constraints)
2. The Metal agent correctly starting the native llama-server process

This caused deployment failures on resource-constrained clusters like minikube, even though the Metal deployment was actually working.

**Example error seen:**
```
Error: inference service deployment failed
Waiting for deployment to be ready (timeout: 10m0s)...
[2s] Model: Downloading, Service: Pending (0/0 replicas)
[38s] Model: Ready, Service: Failed (0/1 replicas)

Pod Status: Pending
Reason: 0/1 nodes are available: 1 Insufficient memory
```

#### Solution
The controller now detects the Metal accelerator in the Model spec and:
- **Skips** creating containerized Deployment entirely
- Only creates the Service resource for Kubernetes service discovery
- Lets the Metal agent handle running llama-server natively on the host

**Changes:**
- Modified `internal/controller/inferenceservice_controller.go` to check for Metal accelerator before Deployment creation
- Properly handles ready replica count for Metal vs containerized deployments
- Added clear logging: "Metal accelerator detected, skipping Deployment creation"

#### Impact
- Metal quickstart guide now works flawlessly on minikube with default 8GB RAM
- InferenceService status correctly shows "Ready" instead of "Failed"
- No more confusing "Insufficient memory" errors when Metal agent is working fine
- Better user experience for macOS developers

## ✨ Features

### Automatic Version Update Notifications

**New: CLI checks for updates and notifies users when newer versions are available**

The CLI now automatically checks for new releases and displays a notification when an update is available.

#### How It Works

**On every command invocation:**
- Checks GitHub Releases API for the latest version
- Uses 24-hour cache to avoid performance impact
- Gracefully handles offline scenarios (fails silently)
- Only shows notification if a newer version exists

**Example output:**
```bash
$ llmkube deploy llama-3.1-8b

⚠️  New version available: v0.3.2 (current: v0.3.1)
   Update with: brew upgrade llmkube
   Or download from: https://github.com/defilantech/LLMKube/releases/latest

📚 Using catalog model: Llama 3.1 8B Instruct
...
```

#### Manual Version Check

Use the new `--check` flag with the version command:

```bash
$ llmkube version --check
llmkube version 0.3.1
  git commit: unknown
  build date: unknown

Checking for updates...
  ⚠️  New version available: v0.3.2
     Update with: brew upgrade llmkube
     Or download from: https://github.com/defilantech/LLMKube/releases/latest
```

Or if you're up to date:
```bash
$ llmkube version --check
llmkube version 0.3.2
  git commit: fb3adf5
  build date: 2025-11-24

Checking for updates...
  ✅ You're running the latest version!
```

#### Cache Details
- **Location**: `~/.llmkube/version_cache.json`
- **Expiration**: 24 hours
- **Offline behavior**: Silently skips check if GitHub API is unreachable
- **Privacy**: Only queries public GitHub API, no telemetry

#### Benefits
- Users stay informed about new releases
- Reduces support burden from users running outdated versions
- No performance impact (cached for 24 hours)
- Works offline gracefully

**Technical Details:**
- New utility: `pkg/cli/version_check.go`
- Added `PersistentPreRun` hook to root command
- Enhanced version command with `--check` flag
- Uses GitHub Releases API for version comparison

## 🛠️ Technical Details

### Files Changed
- `internal/controller/inferenceservice_controller.go` - Metal accelerator detection
- `pkg/cli/version_check.go` - Version checking utility (new)
- `pkg/cli/root.go` - Added PersistentPreRun hook
- `pkg/cli/version.go` - Enhanced with --check flag

### Testing
- ✅ Controller compiles and all tests pass
- ✅ Metal deployments work on minikube (8GB RAM)
- ✅ Version check verified with GitHub API
- ✅ Cache functionality confirmed (24-hour expiration)
- ✅ Offline handling tested (no errors when disconnected)
- ✅ Linter passes (errcheck for resp.Body.Close)

## 📦 Installation & Upgrade

### Upgrading from v0.3.1 or earlier

#### macOS (Homebrew)
```bash
brew upgrade llmkube
```

#### Manual Installation

**macOS (Apple Silicon):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.2/LLMKube_0.3.2_darwin_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**macOS (Intel):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.2/LLMKube_0.3.2_darwin_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Linux (amd64):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.2/LLMKube_0.3.2_linux_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Linux (arm64):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.3.2/LLMKube_0.3.2_linux_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

### No Controller Changes
The controller version remains compatible. Only the CLI binary needs to be updated.

## 🧪 Verification

After upgrading, verify the fixes:

### Test Metal Deployment
```bash
# Should work without memory errors on minikube (8GB RAM)
llmkube deploy llama-3.1-8b --accelerator metal

# Check status - should show Ready
llmkube status llama-3.1-8b

# Verify Metal agent is handling it
tail -f /tmp/llmkube-metal-agent.log
```

### Test Version Check
```bash
# Check current version
llmkube version

# Manual update check
llmkube version --check

# Verify cache file created
ls -la ~/.llmkube/version_cache.json
```

## 🔄 Upgrade Impact

**No Breaking Changes** - v0.3.2 is fully backward compatible with v0.3.x.

- All existing deployments continue to work unchanged
- Metal deployments that were "Failed" will reconcile to "Ready"
- No configuration changes required
- No operator/controller restart needed

## 📝 Full Changelog

### Bug Fixes
- Fixed Metal deployments creating unnecessary containerized pods (#41, #42)
- Metal accelerator now properly skips Deployment creation
- InferenceService status correctly shows "Ready" for Metal deployments
- Fixed errcheck linter warning for resp.Body.Close

### Features
- Added automatic version update notifications
- CLI checks for new versions daily (with cache)
- Added `llmkube version --check` for manual update checks
- Version cache stored in `~/.llmkube/version_cache.json`

### Improvements
- Better handling of resource-constrained clusters with Metal
- Graceful offline handling for version checks
- Clearer logging for Metal deployment flow
- Reduced confusion in Metal quickstart experience

## 📊 What's Next

### v0.3.3 (Planned)
- Additional Metal stability improvements
- Enhanced error messages and troubleshooting
- Documentation updates

### v0.4.0 (Future)
- Multi-GPU single-node support
- Enhanced monitoring and observability
- Production hardening features

See [ROADMAP.md](ROADMAP.md) for complete roadmap.

## 🔗 Resources

- **Issue #41**: [Controller creates containerized Deployment for Metal accelerator](https://github.com/defilantech/LLMKube/issues/41)
- **PR #42**: [Fix Metal deployment conflict and add version check](https://github.com/defilantech/LLMKube/pull/42)
- **Metal Quick Start**: [examples/metal-quickstart/README.md](examples/metal-quickstart/README.md)
- **Documentation**: [README.md](README.md)
- **Roadmap**: [ROADMAP.md](ROADMAP.md)

## 💬 Community & Support

- **GitHub Issues**: [Report bugs or request features](https://github.com/defilantech/LLMKube/issues)
- **Discussions**: [Ask questions and share ideas](https://github.com/defilantech/LLMKube/discussions)

---

**Version**: v0.3.2
**Release Date**: November 24, 2025
**License**: Apache 2.0

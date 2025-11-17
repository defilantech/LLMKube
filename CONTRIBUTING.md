# Contributing to LLMKube

Thank you for your interest in contributing to LLMKube! This project aims to make local LLM deployment as reliable and scalable as deploying microservices on Kubernetes.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [How Can I Contribute?](#how-can-i-contribute)
- [Development Setup](#development-setup)
- [Making Changes](#making-changes)
- [Pull Request Process](#pull-request-process)
- [Coding Standards](#coding-standards)
- [Community](#community)

## Code of Conduct

### Our Pledge

We are committed to providing a welcoming and inspiring community for all. Please be respectful and constructive in all interactions.

**Expected Behavior:**
- Use welcoming and inclusive language
- Be respectful of differing viewpoints and experiences
- Gracefully accept constructive criticism
- Focus on what is best for the community
- Show empathy towards other community members

**Unacceptable Behavior:**
- Harassment, discriminatory language, or personal attacks
- Trolling, insulting/derogatory comments
- Public or private harassment
- Publishing others' private information without permission

## How Can I Contribute?

### Reporting Bugs

Before creating bug reports, please check existing issues to avoid duplicates.

**Good bug reports include:**
- Clear, descriptive title
- Exact steps to reproduce the problem
- Expected vs actual behavior
- LLMKube version (`llmkube version`)
- Kubernetes version (`kubectl version`)
- Environment details (GKE, EKS, minikube, etc.)
- Relevant logs from controller or inference pods
- Sample YAML manifests (if applicable)

**Use the bug report template** when creating issues.

### Suggesting Features

We track feature requests via GitHub Issues with the `enhancement` label.

**Good feature requests include:**
- Clear use case and problem statement
- Proposed solution (if you have one)
- Alternatives you've considered
- Impact on existing functionality
- Examples of similar features in other projects

### First-Time Contributors

Look for issues labeled:
- `good-first-issue` - Small, well-defined tasks
- `help-wanted` - Larger tasks where we need help
- `documentation` - Documentation improvements

### Areas We Need Help

**High Priority (Phase 2+):**
- Multi-platform CLI builds (GoReleaser workflow improvements)
- Multi-GPU single-node support (13B models on 2x GPUs)
- AMD GPU support (ROCm integration)
- Intel GPU support (oneAPI)
- Additional Grafana dashboards (inference metrics, cost tracking)
- Performance benchmarking on different GPU types

**Medium Priority:**
- Multi-node GPU sharding (70B models across multiple nodes)
- Edge deployment guides (K3s, ARM64, Jetson)
- Helm chart packaging
- Advanced SLO auto-remediation
- Cost optimization features

**Completed (Phase 0-1):**
- ✅ NVIDIA GPU support with CUDA
- ✅ Prometheus + Grafana + DCGM metrics
- ✅ GPU E2E test suite
- ✅ GPU dashboard templates
- ✅ CLI with GPU deployment support

## Development Setup

### Prerequisites

**Required:**
- **Go 1.24+**: Install from [golang.org](https://golang.org/dl/)
- **Docker 17.03+**: For building images
- **kubectl**: Configured with a cluster (minikube, kind, GKE, etc.)
- **Kubernetes cluster**: v1.11.3+ (minikube, kind, or cloud)

**Optional (for GPU development):**
- **GPU cluster**: GKE with NVIDIA GPUs (see `terraform/gke/`)
- **NVIDIA GPU Operator**: Installed on cluster
- **CUDA knowledge**: For GPU inference work

### Clone and Build

```bash
# Fork the repository on GitHub first, then:
git clone git@github.com:YOUR_USERNAME/llmkube.git
cd llmkube

# Install dependencies
go mod download

# Generate CRD manifests
make manifests

# Build the operator binary
make build

# Build the CLI
make build-cli

# Run tests
make test
```

### Running Locally

```bash
# Install CRDs into your cluster
make install

# Run the controller locally (outside cluster)
make run

# In another terminal, deploy a test model
kubectl apply -f config/samples/inference_v1alpha1_model.yaml
kubectl apply -f config/samples/inference_v1alpha1_inferenceservice.yaml
```

### Running in Cluster

```bash
# Build and push Docker image
export IMG=<your-registry>/llmkube:dev
make docker-build docker-push IMG=$IMG

# Deploy to cluster
make deploy IMG=$IMG

# Check controller logs
kubectl logs -n llmkube-system deployment/llmkube-controller-manager -f
```

## Making Changes

### Branching Strategy

- `main` - Stable, production-ready code
- `feat/*` - New features
- `fix/*` - Bug fixes
- `docs/*` - Documentation changes
- `refactor/*` - Code refactoring

**Example:**
```bash
git checkout -b feat/prometheus-metrics
```

### Commit Messages

We follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <subject>

<body>

<footer>
```

**Types:**
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation only
- `refactor`: Code refactoring (no functional change)
- `test`: Adding tests
- `chore`: Build process, tooling changes

**Examples:**
```
feat(controller): Add Prometheus metrics exporter

Adds token/sec, latency, and request count metrics to the
InferenceService controller. Metrics are exposed on :8080/metrics.

Closes #42
```

```
fix(model): Handle missing GGUF metadata gracefully

Previously crashed when GGUF files had incomplete metadata.
Now logs warning and continues with defaults.

Fixes #55
```

### Code Changes

1. **Write tests** for new functionality
2. **Update documentation** (README, code comments)
3. **Run linters**: `make lint`
4. **Run tests**: `make test`
5. **Test manually** in a real cluster

### Testing Guidelines

```bash
# Unit tests
make test

# E2E tests (requires cluster)
make test-e2e

# GPU E2E tests (requires GPU cluster)
./test/e2e/gpu_test.sh

# Lint
make lint

# Verify manifests are up to date
make manifests
git diff --exit-code  # Should be no changes
```

**Writing Tests:**
- Use table-driven tests for multiple cases
- Test both success and error paths
- Mock external dependencies (K8s API, HTTP downloads)
- Test CRD validation and defaulting
- Add GPU-specific tests for GPU features
- Verify metrics are properly exported

## Pull Request Process

### Before Submitting

- [ ] Code passes all tests (`make test`)
- [ ] Linter passes (`make lint`)
- [ ] Manually tested in a Kubernetes cluster
- [ ] GPU tests pass (if GPU-related: `./test/e2e/gpu_test.sh`)
- [ ] Documentation updated (if adding features)
- [ ] Commit messages follow conventions
- [ ] Branch is up-to-date with `main`
- [ ] No hardcoded credentials or project IDs

### Submitting a PR

1. **Push your branch** to your fork
2. **Open a Pull Request** against `main`
3. **Fill out the PR template** completely
4. **Link related issues** (e.g., "Closes #42")
5. **Request review** from maintainers

### PR Title Format

```
<type>: <description>
```

**Examples:**
- `feat: Add Prometheus metrics to InferenceService controller`
- `fix: Resolve model download timeout on slow connections`
- `docs: Add troubleshooting guide for GPU issues`

### Review Process

1. **Automated checks** run (tests, linting, builds)
2. **Maintainer review** (usually within 2-3 days)
3. **Address feedback** by pushing new commits
4. **Approval and merge** (squash-merge to `main`)

**What reviewers look for:**
- Code quality and readability
- Test coverage
- Performance implications
- Backward compatibility
- Documentation completeness

## Coding Standards

### Go Code Style

- Follow [Effective Go](https://golang.org/doc/effective_go.html)
- Use `gofmt` and `golangci-lint`
- Keep functions small and focused
- Add comments for exported types/functions
- Use descriptive variable names

**Example:**
```go
// ReconcileModel downloads and validates the model specified in the Model CRD.
// It updates the Model status with download progress and any errors encountered.
func (r *ModelReconciler) ReconcileModel(ctx context.Context, model *inferencev1alpha1.Model) error {
    // Implementation
}
```

### CRD Design

- Follow [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
- Use `+kubebuilder` markers for validation
- Provide meaningful status conditions
- Add examples in `config/samples/`

### Documentation

- **Code comments**: Explain *why*, not *what*
- **README updates**: For user-facing changes
- **Architecture docs**: For design decisions
- **Examples**: Working YAML manifests

## Community

### Communication Channels

- **GitHub Issues**: Bug reports, feature requests
- **GitHub Discussions**: Q&A, ideas, general discussion (coming soon)
- **Slack/Discord**: Real-time chat (coming soon)

### Weekly Sync

**When**: Mondays, 10am PT (details in Discord when available)
**Agenda**: Review PRs, discuss roadmap, unblock contributors

### Recognition

Contributors will be:
- Listed in release notes
- Added to CONTRIBUTORS file
- Thanked in community updates

## Development Workflow Example

Here's a complete workflow for adding a feature:

```bash
# 1. Create feature branch
git checkout -b feat/multi-gpu-single-node

# 2. Make changes
vim api/v1alpha1/model_types.go
vim internal/controller/inferenceservice_controller.go

# 3. Generate manifests
make manifests

# 4. Write tests
vim internal/controller/inferenceservice_controller_test.go

# 5. Run tests
make test

# 6. Test manually (CPU)
make install
make run
kubectl apply -f config/samples/inference_v1alpha1_model.yaml

# 7. Test with GPU (if GPU feature)
kubectl apply -f config/samples/gpu-model-example.yaml
./test/e2e/gpu_test.sh

# 8. Lint
make lint

# 9. Update documentation
vim README.md
vim docs/gpu-setup-guide.md  # If GPU-related

# 10. Commit
git add .
git commit -m "feat(gpu): Add multi-GPU single-node support

Enables layer offloading across multiple GPUs on single node.
Adds spec.hardware.gpu.count field to Model CRD.
Verified with 13B model on 2x L4 GPUs.

Closes #123"

# 11. Push and create PR
git push origin feat/multi-gpu-single-node
# Open PR on GitHub
```

## Questions?

If you're stuck or have questions:
- Check existing documentation (README, ROADMAP, code comments)
- Search closed issues and PRs
- Ask in GitHub Discussions (when available)
- Ping maintainers in your PR

## License

By contributing to LLMKube, you agree that your contributions will be licensed under the Apache License 2.0.

---

**Thank you for contributing to LLMKube!** Every PR, issue report, and doc improvement helps make local LLM deployment accessible to everyone.

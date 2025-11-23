# GitHub Actions Workflows

## Helm Chart CI

**File**: `helm-chart.yml`

Comprehensive testing for the LLMKube Helm chart. Runs on:
- Push to `main` or `develop` branches (when chart files change)
- Pull requests that modify chart files

### Test Jobs

#### 1. Lint and Validate (lint-and-validate)
- ✅ Helm lint check
- ✅ Chart version validation
- ✅ AppVersion validation

**Duration**: ~30 seconds

#### 2. Template Validation (template-validation)
Matrix testing with different values configurations:
- Default values
- Basic configuration (`values-basic.yaml`)
- Production configuration (`values-production.yaml`)
- GPU cluster configuration (`values-gpu-cluster.yaml`)

Each variant:
- ✅ Renders templates successfully
- ✅ Validates Kubernetes manifests with kubeconform
- ✅ Ensures minimum resource count

**Duration**: ~1-2 minutes

#### 3. Prometheus Integration (prometheus-integration)
- ✅ ServiceMonitor renders when enabled
- ✅ PrometheusRule renders when enabled
- ✅ GPU alerts are present
- ✅ Custom thresholds apply correctly

**Duration**: ~45 seconds

#### 4. CRD Validation (crd-validation)
- ✅ CRDs render when `crds.install=true`
- ✅ CRDs omitted when `crds.install=false`
- ✅ Both Model and InferenceService CRDs present
- ✅ CRD names are correct

**Duration**: ~30 seconds

#### 5. Package and Install Test (package-test)
End-to-end testing on a real Kubernetes cluster:
- ✅ Chart packages successfully
- ✅ Dry-run installation succeeds
- ✅ Installs on kind cluster
- ✅ All resources deployed correctly
- ✅ Helm upgrade works
- ✅ CRDs preserved on uninstall

**Duration**: ~3-4 minutes

#### 6. Security Scan (security-scan)
Best practices and security validation:
- ✅ No hard-coded secrets
- ✅ Security contexts configured
  - `runAsNonRoot: true`
  - `readOnlyRootFilesystem: true`
  - Capabilities dropped
- ✅ Resource limits set

**Duration**: ~30 seconds

#### 7. Documentation (documentation)
- ✅ Chart README.md exists
- ✅ Essential sections present
- ✅ Example values files exist
- ✅ NOTES.txt contains helpful commands

**Duration**: ~20 seconds

### Total CI Duration
- **Parallel execution**: ~3-4 minutes
- **All jobs**: ~8-10 minutes if run sequentially

### Adding the Badge

Add to your README.md:

```markdown
[![Helm Chart CI](https://github.com/defilantech/LLMKube/actions/workflows/helm-chart.yml/badge.svg)](https://github.com/defilantech/LLMKube/actions/workflows/helm-chart.yml)
```

### Local Testing

Run the same tests locally:

```bash
# Lint
helm lint charts/llmkube

# Template validation
helm template llmkube charts/llmkube --namespace llmkube-system

# Test with different values
helm template llmkube charts/llmkube \
  -f charts/llmkube/examples/values-production.yaml \
  --namespace llmkube-system

# Package
helm package charts/llmkube -d /tmp

# Install dry-run
helm install llmkube /tmp/llmkube-0.2.1.tgz \
  --namespace llmkube-system \
  --dry-run
```

### Skipping CI

To skip CI on a commit (e.g., documentation-only changes):

```bash
git commit -m "docs: Update README [skip ci]"
```

### Debugging Failed Tests

1. **Check the Actions tab** on GitHub
2. **Expand failed job** to see detailed logs
3. **Download artifacts** if available
4. **Run locally** using the commands above

### Adding New Tests

To add new tests:

1. Add a new job to `helm-chart.yml`
2. Follow the existing pattern
3. Use descriptive names and echo statements
4. Test locally before committing

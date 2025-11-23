# Helm Chart Testing Guide

This document shows how to run the same tests locally that run in CI.

## Prerequisites

```bash
# Install Helm
brew install helm

# Install kubeconform (for manifest validation)
brew install kubeconform

# Install kind (for local Kubernetes testing)
brew install kind

# Install yq (for YAML processing)
brew install yq
```

## Quick Test Suite

Run all tests locally before pushing:

```bash
# 1. Lint
echo "Running helm lint..."
helm lint charts/llmkube

# 2. Template rendering (default)
echo "Testing default template rendering..."
helm template llmkube charts/llmkube --namespace llmkube-system > /tmp/llmkube-default.yaml

# 3. Template rendering (all examples)
for values in basic production gpu-cluster; do
  echo "Testing values-${values}.yaml..."
  helm template llmkube charts/llmkube \
    --namespace llmkube-system \
    -f charts/llmkube/examples/values-${values}.yaml \
    > /tmp/llmkube-${values}.yaml
done

# 4. Validate manifests with kubeconform
echo "Validating Kubernetes manifests..."
kubeconform -summary -ignore-missing-schemas -skip CustomResourceDefinition /tmp/llmkube-default.yaml

# 5. Test Prometheus resources
echo "Testing ServiceMonitor..."
helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set prometheus.serviceMonitor.enabled=true | \
  grep -q "kind: ServiceMonitor" && echo "✅ ServiceMonitor OK"

echo "Testing PrometheusRule..."
helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set prometheus.prometheusRule.enabled=true | \
  grep -q "kind: PrometheusRule" && echo "✅ PrometheusRule OK"

# 6. Test CRD installation
echo "Testing CRD installation..."
helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set crds.install=true | \
  grep -q "kind: CustomResourceDefinition" && echo "✅ CRDs enabled OK"

helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set crds.install=false | \
  grep -q "kind: CustomResourceDefinition" && echo "❌ CRDs should be disabled" || echo "✅ CRDs disabled OK"

# 7. Package
echo "Packaging chart..."
helm package charts/llmkube -d /tmp

echo ""
echo "✅ All local tests passed!"
```

## Individual Tests

### 1. Lint Test

```bash
helm lint charts/llmkube
```

Expected: `0 chart(s) failed`

### 2. Template Validation

```bash
# Default values
helm template llmkube charts/llmkube --namespace llmkube-system

# With production values
helm template llmkube charts/llmkube \
  -f charts/llmkube/examples/values-production.yaml \
  --namespace llmkube-system

# Count resources
helm template llmkube charts/llmkube --namespace llmkube-system | grep -c "^# Source:"
# Should be >= 10
```

### 3. Manifest Validation

```bash
# Generate manifests
helm template llmkube charts/llmkube \
  --namespace llmkube-system > /tmp/manifests.yaml

# Validate with kubeconform
kubeconform -summary -ignore-missing-schemas -skip CustomResourceDefinition /tmp/manifests.yaml
```

### 4. Prometheus Integration Tests

```bash
# Test ServiceMonitor
helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set prometheus.serviceMonitor.enabled=true \
  --debug | grep -A 20 "kind: ServiceMonitor"

# Test PrometheusRule
helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set prometheus.prometheusRule.enabled=true \
  --debug | grep -A 30 "kind: PrometheusRule"

# Test custom thresholds
helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set prometheus.prometheusRule.enabled=true \
  --set prometheus.prometheusRule.rules.gpu.highUtilizationThreshold=95 | \
  grep "DCGM_FI_DEV_GPU_UTIL > 95"
```

### 5. CRD Tests

```bash
# With CRDs
helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set crds.install=true | \
  grep "kind: CustomResourceDefinition"

# Without CRDs
helm template llmkube charts/llmkube \
  --namespace llmkube-system \
  --set crds.install=false | \
  grep "kind: CustomResourceDefinition"
# Should return empty
```

### 6. Package Test

```bash
# Package
helm package charts/llmkube -d /tmp

# Verify package
tar -tzf /tmp/llmkube-*.tgz | head -20

# Dry-run install
helm install llmkube-test /tmp/llmkube-*.tgz \
  --namespace llmkube-test \
  --create-namespace \
  --dry-run
```

### 7. Full Integration Test (Requires Kind)

```bash
# Create kind cluster
kind create cluster --name helm-test

# Install chart
helm install llmkube /tmp/llmkube-*.tgz \
  --namespace llmkube-system \
  --create-namespace \
  --wait \
  --timeout 2m

# Verify installation
kubectl get namespace llmkube-system
kubectl get crd models.inference.llmkube.dev
kubectl get crd inferenceservices.inference.llmkube.dev
kubectl get deployment -n llmkube-system
kubectl get serviceaccount -n llmkube-system

# Test upgrade
helm upgrade llmkube /tmp/llmkube-*.tgz \
  --namespace llmkube-system \
  --set controllerManager.resources.limits.cpu=1 \
  --wait

# Test uninstall
helm uninstall llmkube --namespace llmkube-system

# Verify CRDs are kept
kubectl get crd models.inference.llmkube.dev
kubectl get crd inferenceservices.inference.llmkube.dev

# Cleanup
kind delete cluster --name helm-test
```

## Security Tests

```bash
# Check for secrets
echo "Checking for hard-coded secrets..."
grep -r "password\|secret\|token" charts/llmkube/templates/ | grep -v "serviceAccount\|SecretKeyRef" || echo "✅ No secrets found"

# Validate security contexts
echo "Checking security contexts..."
helm template llmkube charts/llmkube --namespace llmkube-system | grep "runAsNonRoot: true" && echo "✅ runAsNonRoot set"
helm template llmkube charts/llmkube --namespace llmkube-system | grep "readOnlyRootFilesystem: true" && echo "✅ readOnlyRootFilesystem set"

# Check resource limits
echo "Checking resource limits..."
helm template llmkube charts/llmkube --namespace llmkube-system | grep -A 10 "kind: Deployment" | grep "limits:" && echo "✅ Resource limits set"
```

## Documentation Tests

```bash
# Check README
test -f charts/llmkube/README.md && echo "✅ README exists"

# Check sections
for section in "Prerequisites" "Installing" "Configuration" "Uninstalling"; do
  grep -q "$section" charts/llmkube/README.md && echo "✅ Section: $section"
done

# Check examples
for example in basic production gpu-cluster; do
  test -f charts/llmkube/examples/values-${example}.yaml && echo "✅ Example: values-${example}.yaml"
done

# Check NOTES.txt
test -f charts/llmkube/templates/NOTES.txt && echo "✅ NOTES.txt exists"
grep -q "kubectl" charts/llmkube/templates/NOTES.txt && echo "✅ NOTES.txt contains kubectl commands"
```

## Running Specific CI Jobs Locally

### Using Act (GitHub Actions locally)

```bash
# Install act
brew install act

# Run specific job
act -j lint-and-validate

# Run all jobs
act

# Run on pull request event
act pull_request
```

## Troubleshooting

### Chart Won't Lint

```bash
# Get detailed output
helm lint charts/llmkube --debug

# Check YAML syntax
yq eval charts/llmkube/values.yaml > /dev/null && echo "✅ values.yaml syntax OK"
yq eval charts/llmkube/Chart.yaml > /dev/null && echo "✅ Chart.yaml syntax OK"
```

### Template Rendering Fails

```bash
# Debug template rendering
helm template llmkube charts/llmkube --namespace llmkube-system --debug

# Check specific template
helm template llmkube charts/llmkube --namespace llmkube-system --show-only templates/deployment.yaml
```

### Manifest Validation Errors

```bash
# Get detailed kubeconform output
kubeconform -verbose -ignore-missing-schemas -skip CustomResourceDefinition /tmp/manifests.yaml
```

## CI/CD Integration

These tests automatically run on:
- Push to `main` or `develop`
- Pull requests that modify `charts/**`

See `.github/workflows/helm-chart.yml` for the full CI configuration.

## Best Practices

1. **Always run tests locally** before pushing
2. **Test with all example values** files
3. **Verify security contexts** are properly set
4. **Check documentation** is up-to-date
5. **Test upgrades** not just installations
6. **Verify CRD handling** (install/keep/remove)

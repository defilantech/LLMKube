---
name: Bug Report
about: Report a bug or unexpected behavior
title: '[BUG] '
labels: bug
assignees: ''
---

## Bug Description

A clear and concise description of what the bug is.

## Steps to Reproduce

1. Deploy model with '...'
2. Run command '...'
3. Observe error '...'

## Expected Behavior

What you expected to happen.

## Actual Behavior

What actually happened.

## Environment

**LLMKube Version:**
```bash
llmkube version
# Output:
```

**Kubernetes Version:**
```bash
kubectl version
# Output:
```

**Cluster Type:**
- [ ] GKE
- [ ] EKS
- [ ] AKS
- [ ] minikube
- [ ] kind
- [ ] K3s
- [ ] Other:

**GPU (if applicable):**
- [ ] NVIDIA T4
- [ ] NVIDIA L4
- [ ] NVIDIA V100
- [ ] NVIDIA A100
- [ ] None (CPU only)
- [ ] Other:

## Logs

**Controller Logs:**
```bash
kubectl logs -n llmkube-system deployment/llmkube-controller-manager --tail=50
# Paste logs here
```

**Inference Pod Logs:**
```bash
kubectl logs <pod-name> -c llama-server
# Paste logs here
```

## YAML Manifests

**Model:**
```yaml
# Paste your Model YAML here
```

**InferenceService:**
```yaml
# Paste your InferenceService YAML here
```

## Additional Context

Add any other context about the problem here (screenshots, error messages, etc.).

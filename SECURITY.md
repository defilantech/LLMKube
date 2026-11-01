# Security Policy

## Supported Versions

We support the latest minor release plus the previous one. Older minors receive security fixes only at our discretion.

| Version | Supported          |
| ------- | ------------------ |
| 0.10.x  | :white_check_mark: |
| 0.9.x   | :white_check_mark: |
| < 0.9   | :x:                |

## Reporting a Vulnerability

We take security seriously. If you discover a security vulnerability in LLMKube, please report it responsibly.

### How to Report

1. **Do NOT open a public GitHub issue** for security vulnerabilities.
2. **Preferred:** use GitHub's [private vulnerability reporting](https://github.com/defilantech/LLMKube/security/advisories/new). Reports stay private, are tracked in one place, and let us publish an advisory once a fix ships.
3. **Alternative:** email **contact@defilan.com**.

### What to Include

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

### Response Timeline

- **Acknowledgment**: Within 48 hours
- **Initial Assessment**: Within 7 days
- **Resolution Target**: Within 30 days for critical issues

### Scope

This security policy applies to:
- LLMKube controller
- Metal agent (`llmkube-metal-agent`, macOS)
- Foreman operator and agents
- CLI (`llmkube`)
- Helm charts
- Container images published to GHCR

### Out of Scope

- Third-party dependencies (report to upstream)
- LLM model vulnerabilities (report to model providers)
- Self-hosted llama.cpp issues (report to llama.cpp project)

## Trust model

The operator builds inference pods from the InferenceService spec and creates them with its own permissions, not the author's. Several spec fields reach the pod directly, merged with the operator's own entries where both exist: `image`, `command`, `args`, `env` (including `valueFrom.secretKeyRef`), `nodeSelector`, `tolerations`, `affinity`, `runtimeClassName`, `extraVolumes`, `extraVolumeMounts`, `securityContext` and `podSecurityContext`. Permission to create or update an InferenceService is therefore equivalent to permission to create a pod with an arbitrary spec in that namespace, including `hostPath` volumes, `privileged: true` and access to any Secret in the namespace, whether or not the author may create pods directly. Model fields reach the same pods (`spec.source` names a node path for local sources, and `spec.sourceSecretRef` loads every key of a namespace Secret into the model downloader), so treat Model write access the same way.

LLMKube does not filter these fields. Kubernetes Pod Security Admission applies to the pods the operator creates, and it is the control that limits them:

- Enforce a Pod Security Admission level of `baseline` or `restricted` on every namespace that holds InferenceServices. Some features need a more permissive level: local model sources (`file://` or an absolute path) and multi-node inference need `privileged`, and the default model cache prep does not pass `restricted`. The full list is in [Multi-tenancy](docs/operations/multi-tenancy.md#inferenceservice-rights-and-pod-security).
- Grant `create`, `update` and `patch` on InferenceServices and Models only to principals you would also trust to create pods in that namespace. The chart's optional `tenant` and `tenant-admin` ClusterRoles include those verbs.

Two settings are easy to read as stronger boundaries than they are:

- `--allowed-host-path-roots` (chart value `modelSource.allowedHostPathRoots`) limits only the hostPath volume the operator generates for a local model source. It does not inspect `spec.extraVolumes`.
- A team-scoped ModelRouter budget is keyed on a request header the caller sends, and the router-proxy does not authenticate callers. It binds only callers that cannot set or change that header, for example because an authenticating gateway in front of the router sets it.

## Security Best Practices

When deploying LLMKube:

1. **Use RBAC**: Restrict who can create InferenceService and Model resources (see [Trust model](#trust-model))
2. **Pod Security Admission**: Enforce `baseline` or `restricted` on inference namespaces
3. **Network Policies**: Isolate inference pods from sensitive workloads
4. **Resource Limits**: Always set CPU/memory limits to prevent DoS
5. **Image Verification**: Use image digests in production Helm values
6. **Air-gapped Models**: Pre-download models for sensitive environments
7. **Apple Silicon hosts**: Run the Metal agent at 0.10.0 or later, list model directories in `--allowed-model-roots`, and leave `--allow-unsafe-extra-args` off unless every InferenceService author is trusted with the Mac. See the [macOS agent security model](deployment/macos/README.md)

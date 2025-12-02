# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 0.4.x   | :white_check_mark: |
| < 0.4   | :x:                |

## Reporting a Vulnerability

We take security seriously. If you discover a security vulnerability in LLMKube, please report it responsibly.

### How to Report

1. **Do NOT open a public GitHub issue** for security vulnerabilities
2. Email security concerns to: **contact@defilan.com** (or open a private security advisory)
3. Use GitHub's [private vulnerability reporting](https://github.com/defilantech/LLMKube/security/advisories/new)

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
- CLI (`llmkube`)
- Helm charts
- Container images published to GHCR

### Out of Scope

- Third-party dependencies (report to upstream)
- LLM model vulnerabilities (report to model providers)
- Self-hosted llama.cpp issues (report to llama.cpp project)

## Security Best Practices

When deploying LLMKube:

1. **Use RBAC**: Restrict who can create InferenceService resources
2. **Network Policies**: Isolate inference pods from sensitive workloads
3. **Resource Limits**: Always set CPU/memory limits to prevent DoS
4. **Image Verification**: Use image digests in production Helm values
5. **Air-gapped Models**: Pre-download models for sensitive environments

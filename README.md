# LLMKube Helm Repository

This is the Helm chart repository for LLMKube, hosted via GitHub Pages.

## Usage

Add the repository:

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update
```

Install LLMKube:

```bash
helm install llmkube llmkube/llmkube \
  --namespace llmkube-system \
  --create-namespace
```

## More Information

- [Main Repository](https://github.com/defilantech/LLMKube)
- [Documentation](https://github.com/defilantech/LLMKube#readme)
- [Chart Source](https://github.com/defilantech/LLMKube/tree/main/charts/llmkube)

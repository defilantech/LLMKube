# LLMKube v0.2.2 Release Notes

**Release Date**: November 23, 2025
**Status**: Model Catalog Preview (Phase 1)
**Codename**: "Catalog Launch" 📚

## Overview

LLMKube v0.2.2 introduces the **Model Catalog** - a curated collection of 10 battle-tested LLM models with optimized settings. This release makes deploying popular models as simple as `llmkube deploy llama-3.1-8b --gpu` - no need to hunt for GGUF URLs or guess optimal configurations.

**TL;DR**: Browse pre-configured models with `llmkube catalog list`, get detailed specs with `llmkube catalog info <model>`, and deploy instantly with smart defaults. All settings can still be overridden for customization.

## 🚀 What's New

### Model Catalog (Phase 1)

#### 📚 Pre-configured Models

**10 Battle-Tested LLMs** spanning small to large sizes:

**Small Models (1-3B)** - Perfect for edge/CPU deployments:
- **Llama 3.2 3B** - Latest small Llama, 128K context
- **Phi-3 Mini** - Microsoft's efficient 3.8B model, 128K context

**Medium Models (7-9B)** - Best balance of performance and cost:
- **Llama 3.1 8B** - Most popular (5M+ downloads), proven production workhorse
- **Mistral 7B** - Fast and efficient general-purpose model
- **Qwen 2.5 Coder 7B** - Specialized for code generation
- **DeepSeek Coder 6.7B** - Advanced code reasoning
- **Gemma 2 9B** - Google's latest, multilingual

**Large Models (13B+)** - Maximum capability:
- **Qwen 2.5 14B** - Advanced reasoning and long-context tasks
- **Mixtral 8x7B** - Mixture-of-experts, 47B total parameters
- **Llama 3.1 70B** - Flagship model for complex reasoning

All models sourced from bartowski's GGUF repositories with Q4_K_M, Q5_K_M, or Q8_0 quantization for optimal quality/size balance.

#### 🛠️ New CLI Commands

**Browse the Catalog**
```bash
# List all available models
llmkube catalog list

# Output example:
# 📚 LLMKube Model Catalog (v1.0)
# ═══════════════════════════════════════════════
# ID                   NAME                  SIZE  QUANT   USE CASE        VRAM
# ──                   ────                  ────  ─────   ────────        ────
# llama-3.1-8b        Llama 3.1 8B Inst...  8B    Q5_K_M  General Purpose 5-8GB
# qwen-2.5-coder-7b   Qwen 2.5 Coder 7B     7B    Q5_K_M  Code Generation 5-8GB
# ...
```

**Filter by Tags**
```bash
# Show only code-focused models
llmkube catalog list --tag code

# Other useful tags: recommended, small, llama, long-context
```

**View Detailed Information**
```bash
# Get comprehensive model details
llmkube catalog info llama-3.1-8b

# Shows:
# - Full description and size
# - Quantization and context size
# - VRAM estimates
# - Resource requirements (CPU, memory, GPU)
# - Use cases and tags
# - Quick deploy commands
# - Homepage and source URLs
```

#### ⚡ One-Command Deployments

**No More URL Hunting**
```bash
# Before (v0.2.1):
llmkube deploy llama-3.1-8b --gpu \
  --source https://huggingface.co/bartowski/Meta-Llama-3.1-8B-Instruct-GGUF/resolve/main/Meta-Llama-3.1-8B-Instruct-Q5_K_M.gguf \
  --quantization Q5_K_M \
  --cpu 4 \
  --memory 8Gi \
  --gpu-layers 33

# Now (v0.2.2):
llmkube deploy llama-3.1-8b --gpu
```

**Smart Defaults Applied Automatically**:
- Model source URL
- Optimal quantization
- Recommended CPU/memory resources
- GPU layer configuration
- GPU memory allocation

**Full Override Support**:
```bash
# Catalog defaults are just starting points
llmkube deploy llama-3.1-8b --gpu \
  --replicas 3 \              # Scale horizontally
  --cpu 8 \                   # Override CPU
  --memory 16Gi \             # Override memory
  --gpu-layers 20             # Custom GPU offloading
```

#### 🎯 Enhanced Developer Experience

**Better Error Messages**
```bash
$ llmkube deploy my-custom-model
Error: model 'my-custom-model' not found in catalog and no --source provided.
Use 'llmkube catalog list' to see available models
```

**Embedded Catalog**
- YAML catalog embedded in CLI binary
- Works offline (no internet needed to browse)
- Consistent across all installations
- Single source of truth

**Help Integration**
```bash
# Deploy help now showcases catalog
llmkube deploy --help

# Examples section shows:
# Deploy from catalog (simplest - recommended!)
llmkube deploy llama-3.1-8b --gpu
llmkube deploy qwen-2.5-coder-7b --gpu

# List available catalog models
llmkube catalog list
```

## 📊 Model Catalog Coverage

| Category | Count | Examples | Use Cases |
|----------|-------|----------|-----------|
| **Small (1-3B)** | 2 | Llama 3.2 3B, Phi-3 Mini | Edge, CPU, rapid iteration |
| **Medium (7-9B)** | 5 | Llama 3.1 8B, Qwen Coder, Mistral | Production, balanced cost/performance |
| **Large (13B+)** | 3 | Llama 70B, Mixtral 8x7B, Qwen 14B | Complex reasoning, research |
| **Code-focused** | 2 | Qwen Coder, DeepSeek Coder | Code generation, debugging |
| **Long-context** | 7 | 128K+ tokens | Document analysis, RAG |

## 🔧 Technical Details

### Catalog Schema

Each model includes:
- **Metadata**: Name, description, size, tags
- **Model Config**: Source URL, quantization, context size
- **Hardware**: GPU layers, accelerator type
- **Resources**: CPU, memory, GPU memory recommendations
- **VRAM Estimates**: Expected GPU memory usage
- **Use Cases**: Tagged applications
- **Links**: Homepage, source repository

### Implementation

**Files Changed**:
- `pkg/cli/catalog.yaml` - 10-model catalog (NEW)
- `pkg/cli/catalog.go` - Catalog loading and commands (NEW)
- `pkg/cli/catalog_test.go` - Unit tests (NEW, 13 functions)
- `test/e2e/catalog_e2e_test.go` - E2E tests (NEW)
- `pkg/cli/deploy.go` - Catalog integration (MODIFIED)
- `pkg/cli/root.go` - Added catalog command (MODIFIED)
- `README.md` - Catalog documentation (MODIFIED)

**Test Coverage**:
- 13 unit test functions
- 50+ test cases
- 100% coverage of catalog functionality
- E2E tests for all CLI commands

## 📈 What's Next

This is **Phase 1** of the Model Catalog roadmap (see [issue #31](https://github.com/defilantech/LLMKube/issues/31)):

**Phase 2 (v0.3.0)** - Smart Defaults:
- Auto-detect GPU type and select optimal settings
- VRAM validation before deployment
- Auto-scaling based on model requirements
- Context size optimization

**Phase 3 (v0.4.0)** - Community Catalog:
- Support custom catalog sources
- Community contributions
- Model versioning
- Update notifications

## 🐛 Bug Fixes

- Fixed linter compliance (errcheck, lll, staticcheck)
- Corrected E2E test binary paths
- Improved line length compliance in catalog code

## 📚 Documentation Updates

- **README**: Added catalog section to features and quick start
- **CHANGELOG**: Comprehensive v0.2.2 entry
- **Deploy Help**: Updated with catalog examples
- **Test Coverage**: Documented testing approach

## 🎯 Migration Guide

**No Breaking Changes** - v0.2.2 is fully backward compatible.

**New Capabilities**:
1. Explore catalog: `llmkube catalog list`
2. View model details: `llmkube catalog info <model-id>`
3. Deploy catalog models: `llmkube deploy <model-id> --gpu`
4. Filter models: `llmkube catalog list --tag <tag>`

**Existing Deployments**:
- Custom `--source` URLs still work
- All flags still accepted
- No changes to existing workflows

## 🔗 Links

- **Repository**: https://github.com/defilantech/LLMKube
- **Issue #31**: https://github.com/defilantech/LLMKube/issues/31
- **PR #32**: https://github.com/defilantech/LLMKube/pull/32
- **Roadmap**: [ROADMAP.md](ROADMAP.md)
- **Changelog**: [CHANGELOG.md](CHANGELOG.md)

## 🙏 Acknowledgments

All models sourced from the excellent GGUF quantizations by [@bartowski](https://huggingface.co/bartowski).

---

**Full Changelog**: [v0.2.1...v0.2.2](https://github.com/defilantech/LLMKube/compare/v0.2.1...v0.2.2)

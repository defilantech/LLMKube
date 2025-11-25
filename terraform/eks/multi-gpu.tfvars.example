# Multi-GPU Configuration for LLMKube Testing
# Use this for testing multi-GPU single-node deployments (Issue #2)

# AWS Region (us-east-1 has best GPU availability)
region = "us-east-1"

# Cluster configuration
cluster_name = "llmkube-multi-gpu-test"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Multi-GPU Node Configuration
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

# Option 1: 4x T4 GPUs (Recommended - Cost-effective)
gpu_instance_type = "g4dn.12xlarge"  # 4x T4, 48 vCPU, 192GB RAM
gpu_type          = "tesla-t4"
gpu_count         = 4

# Option 2: 4x A10G GPUs (Better performance, higher cost)
# Uncomment these lines and comment out Option 1:
# gpu_instance_type = "g5.12xlarge"  # 4x A10G, 48 vCPU, 192GB RAM
# gpu_type          = "nvidia-a10g"
# gpu_count         = 4

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Auto-scaling configuration
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
min_gpu_nodes = 0  # Start at 0 to save money
max_gpu_nodes = 2  # Allow up to 2 nodes (8 total GPUs if both running)

# Use spot instances to save ~70% on cost
use_spot = true

# Disk size (increase for large models)
disk_size_gb = 200  # Larger disk for 13B+ models

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Estimated Costs (us-east-1, Spot pricing):
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 4x T4 Spot (g4dn.12xlarge):  ~$1.15/hr per node (~$850/mo if 24/7)
# 4x A10G Spot (g5.12xlarge):  ~$1.63/hr per node (~$1200/mo if 24/7)
# With auto-scale to 0 and testing workflow: ~$50-150/mo
#
# Note: us-east-1 typically has best GPU availability and pricing

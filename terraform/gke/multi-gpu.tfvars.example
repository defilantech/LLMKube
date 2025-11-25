# Multi-GPU Configuration for LLMKube Testing
# Use this for testing 2-GPU single-node deployments (Issue #2)

# IMPORTANT: Set your GCP project ID
project_id = "YOUR_PROJECT_ID_HERE"  # CHANGE THIS!

# Cluster configuration
cluster_name = "llmkube-multi-gpu-test"
region       = "us-west1"  # Regional cluster, GPU nodes restricted to zones with T4s via node_locations

# Multi-GPU Node Pool Configuration
# Option 1: 2x T4 GPUs (Cost-effective for testing)
gpu_type     = "nvidia-tesla-t4"
gpu_count    = 2  # 2 GPUs per node
machine_type = "n1-standard-8"  # Required for 2x T4

# Option 2: 2x L4 GPUs (Better performance, higher cost)
# Uncomment these lines and comment out the T4 config above:
# gpu_type     = "nvidia-l4"
# gpu_count    = 2  # 2 GPUs per node
# machine_type = "g2-standard-24"  # Required for 2x L4

# Auto-scaling configuration
min_gpu_nodes = 0  # Start at 0 to save money
max_gpu_nodes = 2  # Allow up to 2 nodes (4 total GPUs if both running)

# Use spot instances to save ~70% on cost
use_spot = true

# Disk size (increase if testing large models)
disk_size_gb = 200  # Larger disk for 13B+ models

# Estimated Costs (us-west1):
# - 2x T4 Spot on n1-standard-8: ~$0.70/hr per node (~$500/mo if 24/7)
# - 2x L4 Spot on g2-standard-24: ~$1.40/hr per node (~$1000/mo if 24/7)
# - With auto-scale to 0 and testing workflow: ~$50-150/mo
# Note: us-west1 typically has better GPU availability than us-central1

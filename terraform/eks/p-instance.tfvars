# P Instance Configuration for Immediate Multi-GPU Testing
# Use this to test NOW without waiting for G instance quota approval
# Region: us-west-2 (where you have P instance quota: 44 vCPUs)

# IMPORTANT: Use us-west-2 (you have P instance quota here!)
region = "us-west-2"

# Cluster configuration
cluster_name = "llmkube-p-instance-test"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# P Instance Multi-GPU Configuration (4x V100)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

# p3.8xlarge: 4x V100 GPUs, 32 vCPUs, 244GB RAM
gpu_instance_type = "p3.8xlarge"
gpu_type          = "nvidia-tesla-v100"
gpu_count         = 4

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Auto-scaling configuration
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
min_gpu_nodes = 0  # Start at 0 to save money
max_gpu_nodes = 1  # Only 1 node allowed (44 vCPU quota / 32 vCPU = 1 node max)

# Use spot instances for 70% discount
use_spot = true

# Disk size (V100s can handle large models)
disk_size_gb = 200

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Cost Estimate (us-west-2, Spot pricing):
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# p3.8xlarge Spot (4x V100):  ~$3.67/hr per node
#
# Testing costs:
#   1 hour:  ~$3.67
#   2 hours: ~$7.34
#   3 hours: ~$11.00
#
# With auto-scale to 0: Only pay when actively testing
#
# GPU Comparison:
#   V100: 16GB memory, older but powerful
#   T4:   16GB memory, newer, more efficient (g4dn - need quota)
#   A10G: 24GB memory, best performance (g5 - need quota)
#
# V100s are perfect for validating multi-GPU functionality!
# Once validated, switch to cheaper G instances.
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

# NOTES:
# - Your quota allows 44 vCPUs in us-west-2 for P instances
# - p3.8xlarge uses 32 vCPUs = you can run 1 node
# - This is PERFECT for testing multi-GPU (4 GPUs)!
# - Remember to run 'terraform destroy' when done

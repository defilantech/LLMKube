# Variables for EKS GPU Cluster

variable "region" {
  description = "AWS region for EKS cluster"
  type        = string
  default     = "us-east-1"
}

variable "cluster_name" {
  description = "Name of the EKS cluster"
  type        = string
  default     = "llmkube-gpu-cluster"
}

variable "gpu_instance_type" {
  description = "EC2 instance type for GPU nodes (g4dn.xlarge, g4dn.12xlarge, g5.xlarge, g5.12xlarge, etc.)"
  type        = string
  default     = "g4dn.xlarge"
}

variable "gpu_type" {
  description = "GPU type (tesla-t4 for g4dn, nvidia-a10g for g5)"
  type        = string
  default     = "tesla-t4"
}

variable "gpu_count" {
  description = "Number of GPUs per node (depends on instance type)"
  type        = number
  default     = 1
}

variable "min_gpu_nodes" {
  description = "Minimum number of GPU nodes (0 to save costs when idle)"
  type        = number
  default     = 0
}

variable "max_gpu_nodes" {
  description = "Maximum number of GPU nodes for auto-scaling"
  type        = number
  default     = 2
}

variable "disk_size_gb" {
  description = "Disk size in GB for GPU nodes"
  type        = number
  default     = 100
}

variable "use_spot" {
  description = "Use spot instances for cost savings (~70% discount)"
  type        = bool
  default     = true
}

variable "project_id" {
  description = "GCP Project ID"
  type        = string
}

variable "region" {
  description = "GCP region for the cluster"
  type        = string
  default     = "us-central1"
}

variable "cluster_name" {
  description = "Name of the GKE cluster"
  type        = string
  default     = "llmkube-gpu-cluster"
}

variable "gpu_type" {
  description = "GPU type (nvidia-tesla-t4, nvidia-tesla-v100, nvidia-l4)"
  type        = string
  default     = "nvidia-tesla-t4"
}

variable "gpu_count" {
  description = "Number of GPUs per node"
  type        = number
  default     = 1
}

variable "min_gpu_nodes" {
  description = "Minimum number of GPU nodes (0 to save money when idle)"
  type        = number
  default     = 0
}

variable "max_gpu_nodes" {
  description = "Maximum number of GPU nodes"
  type        = number
  default     = 2
}

variable "machine_type" {
  description = "Machine type for GPU nodes (n1-standard-4 for T4)"
  type        = string
  default     = "n1-standard-4"
}

variable "use_spot" {
  description = "Use spot/preemptible instances (cheaper but can be interrupted)"
  type        = bool
  default     = true
}

variable "disk_size_gb" {
  description = "Disk size in GB for nodes"
  type        = number
  default     = 100
}

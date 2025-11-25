variable "subscription_id" {
  description = "Azure Subscription ID"
  type        = string
}

variable "resource_group_name" {
  description = "Name of the resource group"
  type        = string
  default     = "llmkube-multi-gpu-rg"
}

variable "location" {
  description = "Azure region for the cluster"
  type        = string
  default     = "westus2"
}

variable "cluster_name" {
  description = "Name of the AKS cluster"
  type        = string
  default     = "llmkube-gpu-cluster"
}

variable "kubernetes_version" {
  description = "Kubernetes version"
  type        = string
  default     = "1.28"
}

variable "gpu_vm_size" {
  description = "VM size for GPU nodes (Standard_NC12s_v3 = 2x V100, Standard_NC64as_T4_v3 = 4x T4)"
  type        = string
  default     = "Standard_NC12s_v3"
}

variable "gpu_count" {
  description = "Number of GPUs to use per node (max depends on VM size)"
  type        = number
  default     = 2
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

variable "system_node_count" {
  description = "Number of system nodes (for control plane workloads)"
  type        = number
  default     = 1
}

variable "system_vm_size" {
  description = "VM size for system nodes"
  type        = string
  default     = "Standard_D2s_v3"
}

variable "enable_spot" {
  description = "Use Azure Spot VMs for GPU nodes (cheaper but can be evicted)"
  type        = bool
  default     = true
}

variable "spot_max_price" {
  description = "Max price for spot instances (-1 = pay up to on-demand price)"
  type        = number
  default     = -1
}

variable "disk_size_gb" {
  description = "OS disk size in GB for GPU nodes"
  type        = number
  default     = 200
}

variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default = {
    project     = "llmkube"
    environment = "testing"
    purpose     = "multi-gpu-llm-inference"
  }
}

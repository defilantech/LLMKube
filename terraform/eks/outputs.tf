# Outputs for EKS GPU Cluster

output "cluster_name" {
  description = "EKS cluster name"
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "EKS cluster endpoint"
  value       = module.eks.cluster_endpoint
}

output "cluster_region" {
  description = "AWS region"
  value       = var.region
}

output "connect_command" {
  description = "Command to configure kubectl"
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

output "gpu_node_group" {
  description = "GPU node group details"
  value = {
    instance_type = var.gpu_instance_type
    gpu_type      = var.gpu_type
    gpu_count     = var.gpu_count
    min_nodes     = var.min_gpu_nodes
    max_nodes     = var.max_gpu_nodes
    spot          = var.use_spot
  }
}

output "cluster_info" {
  description = "Quick reference cluster information"
  value = <<-EOT

    ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
    EKS Cluster Ready!
    ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

    Cluster:        ${module.eks.cluster_name}
    Region:         ${var.region}
    GPU Instance:   ${var.gpu_instance_type}
    GPU Type:       ${var.gpu_type}
    GPUs/Node:      ${var.gpu_count}
    Spot Enabled:   ${var.use_spot}

    Connect:
      ${local.connect_cmd}

    Verify GPUs:
      kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"

    ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  EOT
}

locals {
  connect_cmd = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

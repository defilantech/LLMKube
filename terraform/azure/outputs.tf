output "resource_group_name" {
  description = "Resource group name"
  value       = azurerm_resource_group.llmkube.name
}

output "cluster_name" {
  description = "AKS cluster name"
  value       = azurerm_kubernetes_cluster.gpu_cluster.name
}

output "cluster_location" {
  description = "AKS cluster location"
  value       = azurerm_kubernetes_cluster.gpu_cluster.location
}

output "cluster_id" {
  description = "AKS cluster ID"
  value       = azurerm_kubernetes_cluster.gpu_cluster.id
}

output "kube_config" {
  description = "Kubernetes config"
  value       = azurerm_kubernetes_cluster.gpu_cluster.kube_config_raw
  sensitive   = true
}

output "connect_command" {
  description = "Command to connect to the cluster"
  value       = "az aks get-credentials --resource-group ${var.resource_group_name} --name ${var.cluster_name}"
}

output "gpu_node_pool" {
  description = "GPU node pool name"
  value       = azurerm_kubernetes_cluster_node_pool.gpu_pool.name
}

output "gpu_vm_size" {
  description = "GPU VM size being used"
  value       = var.gpu_vm_size
}

output "estimated_hourly_cost" {
  description = "Estimated hourly cost for GPU nodes"
  value = var.enable_spot ? (
    var.gpu_vm_size == "Standard_NC12s_v3" ? "~$0.90/hr per node (spot, 2x V100)" :
    var.gpu_vm_size == "Standard_NC64as_T4_v3" ? "~$1.20/hr per node (spot, 4x T4)" :
    "~$1.00/hr per node (spot)"
    ) : (
    var.gpu_vm_size == "Standard_NC12s_v3" ? "~$4.50/hr per node (on-demand, 2x V100)" :
    var.gpu_vm_size == "Standard_NC64as_T4_v3" ? "~$6.00/hr per node (on-demand, 4x T4)" :
    "~$5.00/hr per node (on-demand)"
  )
}

output "kubernetes_version" {
  description = "Kubernetes version"
  value       = azurerm_kubernetes_cluster.gpu_cluster.kubernetes_version
}

output "verify_gpu_command" {
  description = "Command to verify GPU nodes"
  value       = "kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable.\\\"nvidia\\.com/gpu\\\""
}

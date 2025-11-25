terraform {
  required_version = ">= 1.0"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 3.80"
    }
  }
}

provider "azurerm" {
  features {}
  subscription_id = var.subscription_id
}

# Resource Group
resource "azurerm_resource_group" "llmkube" {
  name     = var.resource_group_name
  location = var.location
  tags     = var.tags
}

# AKS Cluster
resource "azurerm_kubernetes_cluster" "gpu_cluster" {
  name                = var.cluster_name
  location            = azurerm_resource_group.llmkube.location
  resource_group_name = azurerm_resource_group.llmkube.name
  dns_prefix          = var.cluster_name
  kubernetes_version  = var.kubernetes_version

  # Allow deletion without confirmation (important for cost control!)
  lifecycle {
    prevent_destroy = false
  }

  # System node pool (for control plane workloads)
  default_node_pool {
    name                = "system"
    node_count          = var.system_node_count
    vm_size             = var.system_vm_size
    os_disk_size_gb     = 100
    type                = "VirtualMachineScaleSets"
    enable_auto_scaling = true
    min_count           = 1
    max_count           = 3

    # Only run system workloads on this pool
    node_labels = {
      role = "system"
    }

    tags = var.tags
  }

  # Identity for cluster
  identity {
    type = "SystemAssigned"
  }

  # Network profile
  network_profile {
    network_plugin = "azure"
    network_policy = "azure"
  }

  # Auto-upgrade and maintenance
  automatic_channel_upgrade = "stable"
  maintenance_window {
    allowed {
      day   = "Sunday"
      hours = [3]
    }
  }

  tags = var.tags
}

# GPU Node Pool (for LLM inference)
resource "azurerm_kubernetes_cluster_node_pool" "gpu_pool" {
  name                  = "gpupool"
  kubernetes_cluster_id = azurerm_kubernetes_cluster.gpu_cluster.id
  vm_size               = var.gpu_vm_size
  os_disk_size_gb       = var.disk_size_gb

  # Auto-scaling configuration
  enable_auto_scaling = true
  min_count           = var.min_gpu_nodes
  max_count           = var.max_gpu_nodes
  node_count          = var.min_gpu_nodes

  # Spot instance configuration (if enabled)
  priority        = var.enable_spot ? "Spot" : "Regular"
  eviction_policy = var.enable_spot ? "Delete" : null
  spot_max_price  = var.enable_spot ? var.spot_max_price : null

  # Node labels and taints
  node_labels = {
    role       = "gpu"
    gpu-vendor = "nvidia"
  }

  node_taints = [
    "nvidia.com/gpu=present:NoSchedule"
  ]

  tags = merge(var.tags, {
    gpu = "enabled"
  })

  lifecycle {
    ignore_changes = [
      node_count # Managed by autoscaler
    ]
  }
}

# Install NVIDIA GPU device plugin
resource "null_resource" "install_nvidia_plugin" {
  depends_on = [azurerm_kubernetes_cluster_node_pool.gpu_pool]

  provisioner "local-exec" {
    command = <<-EOT
      # Get credentials
      az aks get-credentials \
        --resource-group ${var.resource_group_name} \
        --name ${var.cluster_name} \
        --overwrite-existing

      # Wait for cluster to be ready
      sleep 30

      # Install NVIDIA device plugin for Kubernetes
      kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.14.0/nvidia-device-plugin.yml

      # Verify installation
      echo "Waiting for NVIDIA device plugin to start..."
      kubectl wait --for=condition=ready pod -l name=nvidia-device-plugin-ds -n kube-system --timeout=300s || true
    EOT
  }

  triggers = {
    cluster_id = azurerm_kubernetes_cluster.gpu_cluster.id
  }
}

# Role assignment for cluster to pull from ACR (if needed)
resource "azurerm_role_assignment" "aks_acr_pull" {
  principal_id                     = azurerm_kubernetes_cluster.gpu_cluster.kubelet_identity[0].object_id
  role_definition_name             = "AcrPull"
  scope                            = azurerm_resource_group.llmkube.id
  skip_service_principal_aad_check = true
}

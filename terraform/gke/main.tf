# GKE Cluster
resource "google_container_cluster" "gpu_cluster" {
  name     = var.cluster_name
  location = var.region

  # Allow deletion without extra confirmation (important for cost control!)
  deletion_protection = false

  # We can't create a cluster with no node pool defined, but we want to only use
  # separately managed node pools. So we create the smallest possible default
  # node pool and immediately delete it.
  remove_default_node_pool = true
  initial_node_count       = 1

  # Networking
  network    = "default"
  subnetwork = "default"

  # Release channel for automatic updates
  release_channel {
    channel = "REGULAR"
  }

  # Enable Workload Identity (best practice)
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # Cluster addons
  addons_config {
    http_load_balancing {
      disabled = false
    }
    horizontal_pod_autoscaling {
      disabled = false
    }
  }

  # Maintenance window (updates during low-traffic hours)
  maintenance_policy {
    daily_maintenance_window {
      start_time = "03:00"
    }
  }
}

# CPU Node Pool (for control plane workloads, small and cheap)
resource "google_container_node_pool" "cpu_pool" {
  name       = "cpu-pool"
  location   = var.region
  cluster    = google_container_cluster.gpu_cluster.name
  node_count = 1

  autoscaling {
    min_node_count = 1
    max_node_count = 3
  }

  node_config {
    preemptible  = var.use_spot
    machine_type = "e2-medium"
    disk_size_gb = 50

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform"
    ]

    labels = {
      role = "cpu"
    }

    metadata = {
      disable-legacy-endpoints = "true"
    }
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }
}

# GPU Node Pool (for LLM inference)
resource "google_container_node_pool" "gpu_pool" {
  name     = "gpu-pool"
  location = var.region
  cluster  = google_container_cluster.gpu_cluster.name

  # Restrict to zones where T4 GPUs are available (exclude us-west1-c)
  node_locations = ["us-west1-a", "us-west1-b"]

  # Start with 0 nodes to save money, auto-scale up when needed
  initial_node_count = var.min_gpu_nodes

  autoscaling {
    min_node_count = var.min_gpu_nodes
    max_node_count = var.max_gpu_nodes
  }

  node_config {
    preemptible  = var.use_spot
    machine_type = var.machine_type
    disk_size_gb = var.disk_size_gb

    # GPU configuration
    guest_accelerator {
      type  = var.gpu_type
      count = var.gpu_count
      gpu_driver_installation_config {
        gpu_driver_version = "DEFAULT"
      }
    }

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform"
    ]

    labels = {
      role = "gpu"
    }

    taint {
      key    = "nvidia.com/gpu"
      value  = "present"
      effect = "NO_SCHEDULE"
    }

    metadata = {
      disable-legacy-endpoints = "true"
    }
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }
}

# Install NVIDIA GPU device plugin (required for GPU scheduling)
resource "null_resource" "install_gpu_driver" {
  depends_on = [google_container_node_pool.gpu_pool]

  provisioner "local-exec" {
    command = <<-EOT
      gcloud container clusters get-credentials ${var.cluster_name} --region=${var.region} --project=${var.project_id}
      kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/container-engine-accelerators/master/nvidia-driver-installer/cos/daemonset-preloaded.yaml
    EOT
  }
}

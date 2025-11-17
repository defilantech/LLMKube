output "cluster_name" {
  description = "GKE cluster name"
  value       = google_container_cluster.gpu_cluster.name
}

output "cluster_endpoint" {
  description = "GKE cluster endpoint"
  value       = google_container_cluster.gpu_cluster.endpoint
  sensitive   = true
}

output "cluster_location" {
  description = "GKE cluster location"
  value       = google_container_cluster.gpu_cluster.location
}

output "connect_command" {
  description = "Command to connect to the cluster"
  value       = "gcloud container clusters get-credentials ${var.cluster_name} --region=${var.region} --project=${var.project_id}"
}

output "gpu_node_pool" {
  description = "GPU node pool name"
  value       = google_container_node_pool.gpu_pool.name
}

output "estimated_hourly_cost" {
  description = "Estimated hourly cost (T4 GPU)"
  value       = "~$0.50/hr per GPU node when running (${var.use_spot ? "spot" : "on-demand"})"
}

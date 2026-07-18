output "masters" {
  description = "Master nodes (public IPs)."
  value       = { for s in hcloud_server.master : s.name => s.ipv4_address }
}

output "workers" {
  description = "Worker nodes (public IPs)."
  value       = { for s in hcloud_server.worker : s.name => s.ipv4_address }
}

output "api_endpoint" {
  description = "Kubernetes API endpoint (first master)."
  value       = "https://${hcloud_server.master[0].ipv4_address}:6443"
}

output "s3_bucket" {
  description = "Backup target bucket on Hetzner Object Storage."
  value       = local.bucket_name
}

output "s3_endpoint" {
  description = "Backup target S3 endpoint."
  value       = "https://${var.s3_endpoint}"
}

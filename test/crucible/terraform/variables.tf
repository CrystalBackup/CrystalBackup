# ---------------------------------------------------------------------------
# Naming / placement
# ---------------------------------------------------------------------------

variable "name_prefix" {
  description = "Prefix for every resource name (servers, network, firewall, bucket)."
  type        = string
  default     = "crucible"
}

variable "location" {
  description = "Hetzner Cloud location (fsn1, nbg1, hel1, ...). Keep it in the same region as s3_endpoint for locality."
  type        = string
  default     = "fsn1"
}

variable "labels" {
  description = "Labels stamped on every hcloud resource. scripts/nuke.sh selects on `project` — do not drop it."
  type        = map(string)
  default = {
    project = "crystalbackup-crucible"
  }
}

# ---------------------------------------------------------------------------
# Cluster shape
# ---------------------------------------------------------------------------

variable "master_count" {
  description = "Number of RKE2 servers (etcd + control plane). Keep it odd."
  type        = number
  default     = 3
}

variable "worker_count" {
  description = "Number of RKE2 agents. Each gets an extra raw volume for Ceph OSDs."
  type        = number
  default     = 3
}

variable "master_type" {
  description = "Server type for masters (mon+mgr land here; 8 GB recommended)."
  type        = string
  # cpx32 = 4 vCPU / 8 GB, AMD x86, currently available in fsn1. The newer Intel
  # cx33/cx43 line is Helsinki-only for now; pick a type your location offers
  # (`hcloud datacenter describe <dc>` -> server_types.available).
  default = "cpx32"
}

variable "worker_type" {
  description = "Server type for workers (osd+mds+rgw+longhorn land here; 16 GB recommended)."
  type        = string
  default     = "cpx42" # 8 vCPU / 16 GB, AMD x86
}

variable "image" {
  description = "OS image for all nodes."
  type        = string
  default     = "ubuntu-24.04"
}

variable "ceph_volume_size" {
  description = "Size (GB) of the extra unformatted volume attached to each worker (future Ceph OSD)."
  type        = number
  default     = 40
}

variable "ssh_key_name" {
  description = "Name of an SSH key ALREADY registered in the Hetzner Cloud project (Security > SSH keys)."
  type        = string
  default     = "crystalbackup"
}

# ---------------------------------------------------------------------------
# Private network
# ---------------------------------------------------------------------------

variable "network_cidr" {
  description = "Private network range."
  type        = string
  default     = "10.42.0.0/16"
}

variable "subnet_cidr" {
  description = "Subnet for the nodes (masters get .11+, workers .21+)."
  type        = string
  default     = "10.42.0.0/24"
}

# ---------------------------------------------------------------------------
# RKE2
# ---------------------------------------------------------------------------

variable "rke2_channel" {
  description = "RKE2 release channel (stable, latest, or a minor channel like v1.33)."
  type        = string
  default     = "stable"
}

variable "rke2_version" {
  description = "Exact RKE2 version (e.g. v1.33.4+rke2r1). Empty = newest in rke2_channel."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# S3 (Hetzner Object Storage) — the backup target bucket
# ---------------------------------------------------------------------------

variable "s3_endpoint" {
  description = "Hetzner Object Storage endpoint host (no scheme)."
  type        = string
  default     = "fsn1.your-objectstorage.com"
}

variable "s3_region" {
  description = "Region string sent to the S3 API (Hetzner uses the location name)."
  type        = string
  default     = "fsn1"
}

variable "s3_access_key" {
  description = "Object Storage access key (from .secrets/, via TF_VAR_s3_access_key)."
  type        = string
  sensitive   = true
}

variable "s3_secret_key" {
  description = "Object Storage secret key (from .secrets/, via TF_VAR_s3_secret_key)."
  type        = string
  sensitive   = true
}

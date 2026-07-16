# Crucible — real-conditions e2e infrastructure for Crystal Backup.
# Works with OpenTofu (recommended, provided by ../mise.toml) and Terraform >= 1.6.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.45"
    }
    # Hetzner Object Storage is S3-compatible but NOT part of the hcloud API;
    # the minio provider manages the bucket over the S3 API. (The AWS provider
    # is avoided on purpose: its newer SDKs force request checksums that many
    # S3-compatible endpoints reject.)
    minio = {
      source  = "aminueza/minio"
      version = "~> 3.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
  }
}

# hcloud token comes from the HCLOUD_TOKEN environment variable
# (exported by ../scripts/load-env.sh — never write it to disk here).
provider "hcloud" {}

provider "minio" {
  minio_server   = var.s3_endpoint
  minio_region   = var.s3_region
  minio_user     = var.s3_access_key
  minio_password = var.s3_secret_key
  minio_ssl      = true
}

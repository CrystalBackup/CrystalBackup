# Crucible — real-conditions e2e infrastructure for Crystal Backup.
# Works with OpenTofu (recommended, provided by ../mise.toml) and Terraform >= 1.6.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.45"
    }
    # Hetzner Object Storage bucket is created via the AWS CLI in a null_resource
    # (see s3.tf) — Hetzner's S3 API lacks the bucket-policy/ACL calls the
    # terraform S3 providers rely on, so no S3 provider is used here.
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
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

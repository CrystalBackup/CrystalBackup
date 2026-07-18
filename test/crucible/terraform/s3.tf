# ---------------------------------------------------------------------------
# Backup target: one private bucket on Hetzner Object Storage
# ---------------------------------------------------------------------------
#
# Hetzner Object Storage is S3-compatible but does NOT support the bucket-policy
# / ACL calls the terraform S3 providers use — they return AccessDenied. All we
# need is a plain private bucket, so create it with the AWS CLI instead.
# Credentials come from the ambient AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
# (exported by scripts/load-env.sh); the checksum vars keep the AWS SDK from
# sending the request checksums Hetzner rejects.

# Bucket names are globally unique on Hetzner — randomize the suffix so several
# people can run crucible against their own projects.
resource "random_id" "bucket" {
  byte_length = 3
}

locals {
  bucket_name = "${var.name_prefix}-${random_id.bucket.hex}"
}

resource "null_resource" "backup_bucket" {
  triggers = {
    bucket   = local.bucket_name
    endpoint = "https://${var.s3_endpoint}"
    region   = var.s3_region
  }

  # Create — idempotent: fall back to head-bucket when it already exists.
  provisioner "local-exec" {
    environment = {
      AWS_REQUEST_CHECKSUM_CALCULATION = "when_required"
      AWS_RESPONSE_CHECKSUM_VALIDATION = "when_required"
      AWS_DEFAULT_REGION               = self.triggers.region
    }
    command = <<-EOT
      aws s3api create-bucket --bucket "${self.triggers.bucket}" \
        --endpoint-url "${self.triggers.endpoint}" 2>/dev/null \
      || aws s3api head-bucket --bucket "${self.triggers.bucket}" \
        --endpoint-url "${self.triggers.endpoint}"
    EOT
  }

  # Best-effort teardown — Hetzner requires the bucket be empty first (--force
  # empties it). Never block `mise run down` on it.
  provisioner "local-exec" {
    when       = destroy
    on_failure = continue
    environment = {
      AWS_REQUEST_CHECKSUM_CALCULATION = "when_required"
      AWS_RESPONSE_CHECKSUM_VALIDATION = "when_required"
      AWS_DEFAULT_REGION               = self.triggers.region
    }
    command = "aws s3 rb \"s3://${self.triggers.bucket}\" --endpoint-url \"${self.triggers.endpoint}\" --force || true"
  }
}

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
  bucket_name = "${local.name_prefix}-${random_id.bucket.hex}"
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

  # Teardown — empty, then delete with retries, then SAY SO if it still failed.
  #
  # This used to be a single `aws s3 rb --force || true`, and it leaked a bucket on
  # almost every run: 32 of them accumulated over ten days, 29 of which were
  # perfectly empty. `rb --force` empties the bucket and then immediately asks for
  # DeleteBucket, but Hetzner Object Storage is eventually consistent about a
  # bucket's object count — the delete comes back `BucketNotEmpty` even though
  # ListObjectVersions reports zero objects, zero delete markers and zero multipart
  # uploads. Measured 2026-08-02: the same DeleteBucket succeeded ~30s later, with
  # nothing done in between. So the emptying was never the problem, and the retry is
  # not defensive — it is the operation.
  #
  # `on_failure = continue` stays: a stuck bucket must not block the destruction of
  # six servers that cost real money by the hour. What changes is that the failure is
  # now LOUD and names the bucket, instead of `|| true` swallowing it under a message
  # telling the operator everything was removed.
  provisioner "local-exec" {
    when       = destroy
    on_failure = continue
    environment = {
      AWS_REQUEST_CHECKSUM_CALCULATION = "when_required"
      AWS_RESPONSE_CHECKSUM_VALIDATION = "when_required"
      AWS_DEFAULT_REGION               = self.triggers.region
    }
    command = <<-EOT
      set -u
      bucket="${self.triggers.bucket}"
      endpoint="${self.triggers.endpoint}"

      # Gone already (a previous destroy got this far) — nothing to do, not an error.
      if ! aws s3api head-bucket --bucket "$bucket" --endpoint-url "$endpoint" >/dev/null 2>&1; then
        echo "bucket $bucket: already gone"
        exit 0
      fi

      # Incomplete multipart uploads are invisible to `s3 rm` and DO keep a bucket
      # undeletable for real (unlike the transient count above). Abort them first.
      aws s3api list-multipart-uploads --bucket "$bucket" --endpoint-url "$endpoint" \
        --query 'Uploads[].[Key,UploadId]' --output text 2>/dev/null |
        while read -r key upload_id; do
          [ -n "$${key:-}" ] || continue
          aws s3api abort-multipart-upload --bucket "$bucket" --endpoint-url "$endpoint" \
            --key "$key" --upload-id "$upload_id" >/dev/null 2>&1 || true
        done

      aws s3 rm "s3://$bucket" --endpoint-url "$endpoint" --recursive >/dev/null 2>&1 || true

      # 6 attempts, 15s apart: ~75s of patience against a convergence delay measured
      # at roughly one 30s interval.
      for attempt in 1 2 3 4 5 6; do
        if aws s3api delete-bucket --bucket "$bucket" --endpoint-url "$endpoint" >/dev/null 2>&1; then
          echo "bucket $bucket: deleted (attempt $attempt)"
          exit 0
        fi
        sleep 15
      done

      echo "WARNING: bucket $bucket could NOT be deleted after 6 attempts." >&2
      echo "         It is emptied but still present, and it will keep costing storage." >&2
      echo "         Delete it by hand:" >&2
      echo "           aws s3api delete-bucket --bucket $bucket --endpoint-url $endpoint" >&2
      exit 1
    EOT
  }
}

# ---------------------------------------------------------------------------
# Backup target: one S3 bucket on Hetzner Object Storage
# ---------------------------------------------------------------------------

# Bucket names are unique per region on Hetzner — randomize the suffix so
# several people can run crucible against their own projects.
resource "random_id" "bucket" {
  byte_length = 3
}

resource "minio_s3_bucket" "backup_target" {
  bucket = "${var.name_prefix}-${random_id.bucket.hex}"
  acl    = "private"
}

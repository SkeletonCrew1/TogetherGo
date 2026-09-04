# S3 bucket holding the Terraform state of every root module in this repository.
# This bucket is deliberately not part of any lifecycle that gets destroyed: losing
# it means losing the record of what exists in the account.
resource "aws_s3_bucket" "state" {
  bucket = var.bucket_name

  # A state bucket should survive an accidental `terraform destroy` of the
  # bootstrap module. Emptying and deleting it is a conscious, manual act.
  lifecycle {
    prevent_destroy = true
  }
}

# Versioning is what makes a corrupted or truncated state recoverable.
resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket = aws_s3_bucket.state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Old state versions accumulate on every apply. Keep 90 days of history, which is
# far more than any realistic rollback window, then let them expire.
resource "aws_s3_bucket_lifecycle_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  # The versioning resource must exist first, otherwise noncurrent-version rules
  # are rejected by the API.
  depends_on = [aws_s3_bucket_versioning.state]

  rule {
    id     = "expire-noncurrent-state-versions"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.noncurrent_version_expiration_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

# State locking. PAY_PER_REQUEST because the access pattern is a handful of
# writes per apply; provisioned capacity would cost more than the state itself.
resource "aws_dynamodb_table" "lock" {
  name         = var.lock_table_name
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "LockID"

  attribute {
    name = "LockID"
    type = "S"
  }

  lifecycle {
    prevent_destroy = true
  }
}

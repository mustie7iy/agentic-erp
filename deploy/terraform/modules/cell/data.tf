# The cell's object store (ADR-0016) and the secrets its workloads read
# through the External Secrets Operator as mounted files (ADR-0017, file:
# references). The secrets are created empty: their values are set by an
# operator or by rotation, never by Terraform, so none is in its state.

variable "secrets" {
  description = "Names of the cell's secrets, under erp/<cell>/ (values are set outside Terraform)."
  type        = list(string)
  default     = ["page-token-key", "temporal-codec-key", "temporal-database-password"]
}

resource "aws_s3_bucket" "objects" {
  bucket_prefix = "${local.prefix}-objects-"
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "objects" {
  bucket = aws_s3_bucket.objects.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "objects" {
  bucket = aws_s3_bucket.objects.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.data.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "objects" {
  bucket                  = aws_s3_bucket.objects.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "objects" {
  bucket = aws_s3_bucket.objects.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_policy" "objects" {
  bucket = aws_s3_bucket.objects.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TLSOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.objects.arn, "${aws_s3_bucket.objects.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
  depends_on = [aws_s3_bucket_public_access_block.objects]
}

resource "aws_secretsmanager_secret" "cell" {
  for_each                = toset(var.secrets)
  name                    = "erp/${var.name}/${each.key}"
  kms_key_id              = aws_kms_key.data.arn
  recovery_window_in_days = 30
}

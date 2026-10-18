# The bucket that holds Terraform state for every root (ADR-0035): S3 with
# native locking (use_lockfile, no DynamoDB table), versioned so a broken
# state can be restored, encrypted with a key of its own, never public, and
# reachable only over TLS. State names every resource and some secrets'
# references, so it is protected like the resources themselves.

variable "bucket" {
  description = "Name of the state bucket (globally unique)."
  type        = string
}

variable "admin_role_arns" {
  description = "Roles that administer the bucket's key; the account root always can."
  type        = list(string)
  default     = []
}

variable "state_role_arns" {
  description = "Roles in other accounts whose roots keep state here (the management account's apply role, for envs/organization)."
  type        = list(string)
  default     = []
  validation {
    condition     = alltrue([for arn in var.state_role_arns : can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/", arn))])
    error_message = "state is shared with IAM roles (arn:aws:iam::<account>:role/...), never a whole account"
  }
}

variable "noncurrent_version_days" {
  description = "How long superseded versions of a state file are kept."
  type        = number
  default     = 365
}

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  account_root = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root"
}

resource "aws_kms_key" "state" {
  description             = "Terraform state (ADR-0035)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([{
      Sid       = "AccountAdministers"
      Effect    = "Allow"
      Principal = { AWS = concat([local.account_root], var.admin_role_arns) }
      Action    = "kms:*"
      Resource  = "*"
      }], length(var.state_role_arns) == 0 ? [] : [{
      Sid       = "OtherAccountsUseState"
      Effect    = "Allow"
      Principal = { AWS = var.state_role_arns }
      Action    = ["kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
      Resource  = "*"
    }])
  })
  # Deleting it would make every state file unreadable.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    id     = "superseded-state"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days           = var.noncurrent_version_days
      newer_noncurrent_versions = 10
    }
  }
}

resource "aws_kms_alias" "state" {
  name          = "alias/erp-terraform-state"
  target_key_id = aws_kms_key.state.key_id
}

resource "aws_s3_bucket" "state" {
  bucket = var.bucket
  lifecycle {
    prevent_destroy = true
  }
}

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
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.state.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket                  = aws_s3_bucket.state.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_policy" "state" {
  bucket = aws_s3_bucket.state.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid       = "TLSOnly"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource  = [aws_s3_bucket.state.arn, "${aws_s3_bucket.state.arn}/*"]
        Condition = { Bool = { "aws:SecureTransport" = "false" } }
      },
      # Terraform's S3 backend with encrypt = true and no kms_key_id asks
      # for SSE-S3 on every write, overriding the bucket's default. A write
      # that asks for any encryption but this bucket's KMS key is refused,
      # so a root initialised without -backend-config=kms_key_id fails
      # loudly; a write that asks for none gets the bucket's default.
      {
        Sid       = "KMSOnly"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.state.arn}/*"
        Condition = { StringNotEqualsIfExists = { "s3:x-amz-server-side-encryption" = "aws:kms" } }
      },
      {
        Sid       = "StateKeyOnly"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.state.arn}/*"
        Condition = { StringNotEqualsIfExists = { "s3:x-amz-server-side-encryption-aws-kms-key-id" = aws_kms_key.state.arn } }
      },
      ], length(var.state_role_arns) == 0 ? [] : [{
        Sid       = "OtherAccountsUseState"
        Effect    = "Allow"
        Principal = { AWS = var.state_role_arns }
        Action    = ["s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
        Resource  = [aws_s3_bucket.state.arn, "${aws_s3_bucket.state.arn}/*"]
    }])
  })
  depends_on = [aws_s3_bucket_public_access_block.state]
}

output "bucket" {
  description = "The state bucket, for every root's backend configuration."
  value       = aws_s3_bucket.state.bucket
}

output "kms_key_arn" {
  description = "The key that encrypts the state."
  value       = aws_kms_key.state.arn
}

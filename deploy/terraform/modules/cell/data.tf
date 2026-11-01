# The cell's object store (ADR-0016) and the secrets its workloads read
# through the External Secrets Operator as mounted files (ADR-0017, file:
# references). The secrets are created empty: their values are set by an
# operator or by rotation, never by Terraform, so none is in its state.

variable "secrets" {
  description = "Names of the cell's secrets, under erp/<cell>/ (values are set outside Terraform). argocd-repository is the cell's read-only deploy key, which its Argo CD reads the repository with (ADR-0035); alertmanager-receivers and grafana-oidc are the monitoring add-on's, which only its own role reads (ADR-0043)."
  type        = list(string)
  default     = ["page-token-key", "database-client-password", "temporal-codec-key", "temporal-database-password", "argocd-repository", "alertmanager-receivers", "grafana-oidc"]
}

variable "directory_users" {
  description = "On the environment's home cell, the rds-db:connect resource of each directory login (modules/global directory_users: router, cell, web), which the router's, the server's and erp-web's roles may connect as; none elsewhere."
  type        = map(string)
  default     = {}
  validation {
    condition     = length(var.directory_users) == 0 || (toset(keys(var.directory_users)) == toset(["router", "cell", "web"]) && alltrue([for arn in values(var.directory_users) : can(regex("^arn:aws[a-z-]*:rds-db:[a-z0-9-]+:[0-9]{12}:dbuser:[A-Za-z0-9-]+/[a-z_][a-z0-9_]*$", arn))]))
    error_message = "none, or the router's, the cell's and erp-web's rds-db dbuser ARNs, one login each"
  }
}

variable "global_secrets" {
  description = "On the environment's home cell, the global tier's secrets its External Secrets Operator also reads for the erp-global chart, and their key (modules/global external_secrets); none elsewhere."
  type = object({
    arns        = list(string)
    kms_key_arn = optional(string)
  })
  default = { arns = [] }
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

# Audit anchors and offboarding exports are written with a compliance-mode
# retention (ADR-0016, ADR-0021), which S3 accepts only in a bucket with
# object lock enabled; enabling it sets no default retention, so every
# other object stays as it was.
resource "aws_s3_bucket_object_lock_configuration" "objects" {
  bucket              = aws_s3_bucket.objects.id
  object_lock_enabled = "Enabled"
  depends_on          = [aws_s3_bucket_versioning.objects]
}

# A restored database may point at objects overwritten or deleted since:
# their noncurrent versions are kept as long as the longest point-in-time
# recovery (35 days, ADR-0040). Locked versions stay until their retention
# ends whatever this says.
resource "aws_s3_bucket_lifecycle_configuration" "objects" {
  bucket = aws_s3_bucket.objects.id
  rule {
    id     = "noncurrent-versions"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = local.noncurrent_version_days
    }
  }
  depends_on = [aws_s3_bucket_versioning.objects]
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
  # A pilot light's cluster reads the same secrets in the DR region.
  dynamic "replica" {
    for_each = local.pilot
    content {
      region     = var.dr_region
      kms_key_id = aws_kms_key.dr[replica.key].arn
    }
  }
}

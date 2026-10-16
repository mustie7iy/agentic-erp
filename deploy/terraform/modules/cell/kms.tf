# The cell's keys, all symmetric and rotating:
#   - data: the database's storage, snapshots and Performance Insights, the
#     object store, the secrets;
#   - cluster: Kubernetes secrets, node and workload volumes, logs;
#   - fields: the KMS master of field encryption (ADR-0019); the server's
#     role wraps and unwraps tenant keys with it and nothing else does.

locals {
  administer = {
    Sid       = "AccountAndAdministrators"
    Effect    = "Allow"
    Principal = { AWS = concat([local.account_root], var.admin_role_arns) }
    Action    = "kms:*"
    Resource  = "*"
  }
}

resource "aws_kms_key" "data" {
  description             = "${local.prefix}: database, object store, secrets (ADR-0035)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [local.administer]
  })
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "data" {
  name          = "alias/${local.prefix}-data"
  target_key_id = aws_kms_key.data.key_id
}

resource "aws_kms_key" "cluster" {
  description             = "${local.prefix}: Kubernetes secrets, volumes, logs (ADR-0035)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      local.administer,
      {
        Sid       = "CloudWatchLogs"
        Effect    = "Allow"
        Principal = { Service = "logs.${data.aws_region.current.region}.amazonaws.com" }
        Action    = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"]
        Resource  = "*"
        Condition = {
          ArnLike = { "kms:EncryptionContext:aws:logs:arn" = "arn:${data.aws_partition.current.partition}:logs:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:log-group:*" }
        }
      },
      {
        # Node groups launch encrypted volumes through Auto Scaling's
        # service-linked role.
        Sid       = "AutoScalingVolumes"
        Effect    = "Allow"
        Principal = { AWS = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling" }
        Action    = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey", "kms:CreateGrant"]
        Resource  = "*"
      },
    ]
  })
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "cluster" {
  name          = "alias/${local.prefix}-cluster"
  target_key_id = aws_kms_key.cluster.key_id
}

resource "aws_kms_key" "fields" {
  description             = "${local.prefix}: field encryption master (ADR-0019)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [local.administer]
  })
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "fields" {
  name          = "alias/${local.prefix}-fields"
  target_key_id = aws_kms_key.fields.key_id
}

# Every new EBS volume of the account and region, the nodes' included, is
# encrypted. These settings belong to the account and region, not the
# cell: with several cells in one account and region, one of them manages
# them (manage_ebs_defaults) and the others leave them alone.
variable "manage_ebs_defaults" {
  description = "Whether this cell sets the account's EBS encryption defaults for its region."
  type        = bool
  default     = true
}

resource "aws_ebs_encryption_by_default" "cell" {
  count   = var.manage_ebs_defaults ? 1 : 0
  enabled = true
}

resource "aws_ebs_default_kms_key" "cell" {
  count   = var.manage_ebs_defaults ? 1 : 0
  key_arn = aws_kms_key.cluster.arn
}

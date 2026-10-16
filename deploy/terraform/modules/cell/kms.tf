# The cell's keys, all symmetric and rotating:
#   - data: the database's storage, snapshots and Performance Insights, the
#     object store, the secrets;
#   - cluster: Kubernetes secrets, node and workload volumes, logs;
#   - fields: the KMS master of field encryption (ADR-0019); the server's
#     role wraps and unwraps tenant keys with it and nothing else does:
#     administrators and the account administer it but cannot use it or
#     grant its use, so unwrapping a tenant key outside the server takes
#     a change of the key policy, which CloudTrail records.

locals {
  administer = {
    Sid       = "AccountAndAdministrators"
    Effect    = "Allow"
    Principal = { AWS = concat([local.account_root], var.admin_role_arns) }
    Action    = "kms:*"
    Resource  = "*"
  }
  # Managing a key without using it: no Encrypt, Decrypt, ReEncrypt,
  # GenerateDataKey or CreateGrant.
  administer_only = {
    Sid       = "AdministerOnly"
    Effect    = "Allow"
    Principal = { AWS = concat([local.account_root], var.admin_role_arns) }
    Action = [
      "kms:CreateAlias", "kms:Describe*", "kms:Enable*", "kms:List*", "kms:Put*", "kms:Update*",
      "kms:Revoke*", "kms:Disable*", "kms:Get*", "kms:Delete*", "kms:TagResource", "kms:UntagResource",
      "kms:ScheduleKeyDeletion", "kms:CancelKeyDeletion", "kms:RotateKeyOnDemand",
    ]
    Resource = "*"
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
        # service-linked role, which must exist before KMS accepts it as a
        # principal: the environment root creates it (autoscaling_role_arn).
        Sid       = "AutoScalingVolumes"
        Effect    = "Allow"
        Principal = { AWS = var.autoscaling_role_arn }
        Action    = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"]
        Resource  = "*"
      },
      {
        Sid       = "AutoScalingVolumeGrants"
        Effect    = "Allow"
        Principal = { AWS = var.autoscaling_role_arn }
        Action    = "kms:CreateGrant"
        Resource  = "*"
        Condition = { Bool = { "kms:GrantIsForAWSResource" = "true" } }
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
    Version = "2012-10-17"
    Statement = [
      local.administer_only,
      {
        # ADR-0019 wraps each tenant key with the context {tenant: <id>}.
        Sid       = "ServerWrapsTenantKeys"
        Effect    = "Allow"
        Principal = { AWS = aws_iam_role.workload["server"].arn }
        Action    = ["kms:Encrypt", "kms:Decrypt"]
        Resource  = "*"
        Condition = {
          "ForAllValues:StringEquals" = { "kms:EncryptionContextKeys" = ["tenant"] }
          Null                        = { "kms:EncryptionContext:tenant" = "false" }
        }
      },
      {
        Sid       = "ServerDescribes"
        Effect    = "Allow"
        Principal = { AWS = aws_iam_role.workload["server"].arn }
        Action    = "kms:DescribeKey"
        Resource  = "*"
      },
    ]
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

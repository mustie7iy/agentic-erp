# An environment's global tier (05 §5.4, ADR-0024, ADR-0035): the directory
# database that maps tenants to cells, and the Network Load Balancer in
# front of the router. It lives in the VPC of the environment's home cell;
# the router's pods run on that cell's cluster (the erp-global chart, T40c),
# which registers them in the target group (TargetGroupBinding).
#
# The load balancer passes TLS through at layer 4: the router terminates
# it, so the load balancer holds no certificate and sees no request. It has
# no access logs: NLB access logs cover TLS listeners only, and their bucket
# must use SSE-S3, which the encryption policy refuses; the router logs
# every request instead.

data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

data "aws_region" "current" {}

locals {
  prefix       = "erp-${var.environment}"
  account_root = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root"
}

resource "aws_kms_key" "directory" {
  description             = "${local.prefix}: the global directory (ADR-0035)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AccountAndAdministrators"
        Effect    = "Allow"
        Principal = { AWS = concat([local.account_root], var.admin_role_arns) }
        Action    = "kms:*"
        Resource  = "*"
      },
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
    ]
  })
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "directory" {
  name          = "alias/${local.prefix}-directory"
  target_key_id = aws_kms_key.directory.key_id
}

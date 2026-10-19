# What a cell keeps in its disaster-recovery region (ADR-0040), when it has
# one (dr_region: staging and production): a key of that region, the
# database's backups copied there, and a replica of the object bucket. The
# resources name their region (the AWS provider's per-resource region), so
# the module needs no second provider and a cell without a DR region
# creates none of them.

locals {
  dr = var.dr_region == null ? toset([]) : toset(["dr"])
  # The longest point-in-time recovery: an object a restored row points at
  # must still exist (ADR-0040).
  noncurrent_version_days = 35
  # Copies in the DR region are the fallback when the region and the
  # replica are both lost; a week of them (ADR-0040).
  dr_copy_days = 7
}

resource "aws_kms_key" "dr" {
  for_each                = local.dr
  region                  = var.dr_region
  description             = "${local.prefix}: database backup copies, the bucket's replica and the restore drill's logs in the DR region (ADR-0040)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      local.administer,
      {
        # The restore drill's verification logs there (T41e2).
        Sid       = "CloudWatchLogs"
        Effect    = "Allow"
        Principal = { Service = "logs.${var.dr_region}.amazonaws.com" }
        Action    = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"]
        Resource  = "*"
        Condition = {
          ArnLike = { "kms:EncryptionContext:aws:logs:arn" = "arn:${data.aws_partition.current.partition}:logs:${var.dr_region}:${data.aws_caller_identity.current.account_id}:log-group:*" }
        }
      },
    ]
  })
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "dr" {
  for_each      = local.dr
  region        = var.dr_region
  name          = "alias/${local.prefix}-dr"
  target_key_id = aws_kms_key.dr[each.key].key_id
}

# The database's backups: AWS Backup snapshots the Multi-AZ DB cluster
# every hour into a vault of the home region and copies each snapshot to
# the DR region's vault. RDS's own cross-region replication of automated
# backups serves DB instances only. The home copies are kept a day: the
# cluster's point-in-time recovery already covers the region.

resource "aws_backup_vault" "cell" {
  for_each    = local.dr
  name        = local.prefix
  kms_key_arn = aws_kms_key.data.arn
}

resource "aws_backup_vault" "dr" {
  for_each    = local.dr
  region      = var.dr_region
  name        = "${local.prefix}-dr"
  kms_key_arn = aws_kms_key.dr[each.key].arn
}

resource "aws_backup_plan" "cell" {
  for_each = local.dr
  name     = local.prefix
  rule {
    rule_name         = "hourly-to-dr"
    target_vault_name = aws_backup_vault.cell[each.key].name
    schedule          = "cron(0 * * * ? *)"
    lifecycle {
      delete_after = 1
    }
    copy_action {
      destination_vault_arn = aws_backup_vault.dr[each.key].arn
      lifecycle {
        delete_after = local.dr_copy_days
      }
    }
  }
}

# AWS Backup's RDS snapshots count as manual DB cluster snapshots, whose
# default quota (100 a region) the copies would pass in four days: 24 a day
# kept 7 days in the DR region, 24 kept a day at home. Both regions' quota
# is raised; the name is looked up, so no quota code is guessed.
locals {
  cluster_snapshot_quota = 250
  snapshot_quota_regions = var.dr_region == null ? {} : { home = null, dr = var.dr_region }
}

data "aws_servicequotas_service_quota" "cluster_snapshots" {
  for_each     = local.snapshot_quota_regions
  region       = each.value
  service_code = "rds"
  quota_name   = "Manual DB cluster snapshots"
}

resource "aws_servicequotas_service_quota" "cluster_snapshots" {
  for_each     = local.snapshot_quota_regions
  region       = each.value
  service_code = "rds"
  quota_code   = data.aws_servicequotas_service_quota.cluster_snapshots[each.key].quota_code
  value        = max(local.cluster_snapshot_quota, data.aws_servicequotas_service_quota.cluster_snapshots[each.key].value)
}

# AWS's managed policy for backup jobs: the baseline AWS Backup documents,
# kept rather than a copy that would fall behind the services it covers.
resource "aws_iam_role" "backup" {
  for_each = local.dr
  name     = "${local.prefix}-backup"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "backup.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "backup" {
  for_each   = local.dr
  role       = aws_iam_role.backup[each.key].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForBackup"
}

resource "aws_backup_selection" "database" {
  for_each     = local.dr
  name         = "${local.prefix}-database"
  plan_id      = aws_backup_plan.cell[each.key].id
  iam_role_arn = aws_iam_role.backup[each.key].arn
  resources    = aws_rds_cluster.cell[*].arn
}

# The bucket's replica: versions and delete markers replicated, so a
# deletion at home is a noncurrent version in both; object lock enabled,
# which S3 requires to replicate locked objects.

resource "aws_s3_bucket" "replica" {
  for_each            = local.dr
  region              = var.dr_region
  bucket_prefix       = "${local.prefix}-replica-"
  object_lock_enabled = true
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "replica" {
  for_each = local.dr
  region   = var.dr_region
  bucket   = aws_s3_bucket.replica[each.key].id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "replica" {
  for_each = local.dr
  region   = var.dr_region
  bucket   = aws_s3_bucket.replica[each.key].id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.dr[each.key].arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "replica" {
  for_each                = local.dr
  region                  = var.dr_region
  bucket                  = aws_s3_bucket.replica[each.key].id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "replica" {
  for_each = local.dr
  region   = var.dr_region
  bucket   = aws_s3_bucket.replica[each.key].id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "replica" {
  for_each = local.dr
  region   = var.dr_region
  bucket   = aws_s3_bucket.replica[each.key].id
  rule {
    id     = "noncurrent-versions"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = local.noncurrent_version_days
    }
  }
  depends_on = [aws_s3_bucket_versioning.replica]
}

resource "aws_s3_bucket_policy" "replica" {
  for_each = local.dr
  region   = var.dr_region
  bucket   = aws_s3_bucket.replica[each.key].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TLSOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.replica[each.key].arn, "${aws_s3_bucket.replica[each.key].arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
  depends_on = [aws_s3_bucket_public_access_block.replica]
}

resource "aws_iam_role" "replication" {
  for_each = local.dr
  name     = "${local.prefix}-replication"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "s3.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "replication" {
  for_each = local.dr
  role     = aws_iam_role.replication[each.key].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["s3:GetReplicationConfiguration", "s3:ListBucket"]
        Resource = aws_s3_bucket.objects.arn
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObjectVersionForReplication", "s3:GetObjectVersionAcl", "s3:GetObjectVersionTagging", "s3:GetObjectRetention", "s3:GetObjectLegalHold"]
        Resource = "${aws_s3_bucket.objects.arn}/*"
      },
      {
        Effect   = "Allow"
        Action   = ["s3:ReplicateObject", "s3:ReplicateDelete", "s3:ReplicateTags"]
        Resource = "${aws_s3_bucket.replica[each.key].arn}/*"
      },
      {
        Effect   = "Allow"
        Action   = "kms:Decrypt"
        Resource = aws_kms_key.data.arn
      },
      {
        Effect   = "Allow"
        Action   = ["kms:Encrypt", "kms:GenerateDataKey"]
        Resource = aws_kms_key.dr[each.key].arn
      },
    ]
  })
}

resource "aws_s3_bucket_replication_configuration" "objects" {
  for_each = local.dr
  bucket   = aws_s3_bucket.objects.id
  role     = aws_iam_role.replication[each.key].arn
  rule {
    id     = "to-dr"
    status = "Enabled"
    filter {}
    delete_marker_replication {
      status = "Enabled"
    }
    source_selection_criteria {
      sse_kms_encrypted_objects {
        status = "Enabled"
      }
    }
    destination {
      bucket = aws_s3_bucket.replica[each.key].arn
      encryption_configuration {
        replica_kms_key_id = aws_kms_key.dr[each.key].arn
      }
    }
  }
  depends_on = [aws_s3_bucket_versioning.objects, aws_s3_bucket_versioning.replica]
}

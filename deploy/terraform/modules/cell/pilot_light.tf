# A production cell's pilot light in its disaster-recovery region (ADR-0040,
# T41b2): a cross-region read replica of the database, a VPC with its
# endpoints, an EKS control plane without nodes, the cluster key's
# counterpart, the field key replicated (a multi-Region key, so tenant keys
# wrapped at home unwrap there) and the secrets replicated. On a region
# failure the runbook promotes the replica and Argo CD syncs the cell there.
# Whether a Multi-AZ DB cluster accepts a cross-region replica is to be
# confirmed on AWS staging before the pilot: the replica is one resource to
# replace if it does not.

variable "pilot_light" {
  description = "Keep a pilot light in dr_region (production cells); staging has the backup copies only."
  type        = bool
  default     = false
  validation {
    condition     = !var.pilot_light || var.dr_region != null
    error_message = "a pilot light needs a dr_region"
  }
}

variable "dr_cidr" {
  description = "The DR VPC's /16, used by no other cell or environment; required with pilot_light."
  type        = string
  default     = null
  validation {
    condition     = !var.pilot_light || var.dr_cidr != null
    error_message = "a pilot light needs a dr_cidr"
  }
}

variable "dr_availability_zones" {
  description = "Three availability zones of dr_region; required with pilot_light."
  type        = list(string)
  default     = []
  validation {
    condition     = !var.pilot_light || length(var.dr_availability_zones) == 3
    error_message = "a pilot light needs three availability zones of dr_region"
  }
}

variable "replica_lag_alarm_actions" {
  description = "Where the replica-lag alarm notifies (SNS topic ARNs of dr_region)."
  type        = list(string)
  default     = []
}

locals {
  pilot = var.pilot_light ? toset(["dr"]) : toset([])
  # The RPO: the replica may be this far behind (ADR-0040).
  replica_lag_seconds = 60
}

module "dr_network" {
  source             = "../network"
  for_each           = local.pilot
  region             = var.dr_region
  name               = "${local.prefix}-dr"
  cidr               = var.dr_cidr
  availability_zones = var.dr_availability_zones
  zone_redundant     = true
}

resource "aws_kms_key" "dr_cluster" {
  for_each                = local.pilot
  region                  = var.dr_region
  description             = "${local.prefix}: Kubernetes secrets and logs in the DR region (ADR-0040)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      local.administer,
      {
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

# The field key's replica: the same key material and id, so the server
# unwraps the tenant keys there; its policy is the primary's.
resource "aws_kms_replica_key" "fields" {
  for_each                = local.pilot
  region                  = var.dr_region
  description             = "${local.prefix}: field encryption master, DR replica (ADR-0019, ADR-0040)"
  primary_key_arn         = aws_kms_key.fields.arn
  deletion_window_in_days = 30
  policy                  = aws_kms_key.fields.policy
  lifecycle {
    prevent_destroy = true
  }
}

# The database's replica.

resource "aws_db_subnet_group" "dr" {
  for_each   = local.pilot
  region     = var.dr_region
  name       = "${local.prefix}-dr"
  subnet_ids = module.dr_network[each.key].private_subnet_ids
}

resource "aws_security_group" "dr_database" {
  for_each    = local.pilot
  region      = var.dr_region
  name        = "${local.prefix}-dr-database"
  description = "PostgreSQL from inside the DR VPC"
  vpc_id      = module.dr_network[each.key].vpc_id
  ingress {
    description = "PostgreSQL from the DR VPC"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [module.dr_network[each.key].cidr]
  }
}

resource "aws_db_parameter_group" "dr" {
  for_each = local.pilot
  region   = var.dr_region
  name     = "${local.prefix}-dr-postgres17"
  family   = "postgres17"
  dynamic "parameter" {
    for_each = local.database_parameters
    content {
      name  = parameter.key
      value = parameter.value
    }
  }
}

resource "aws_db_instance" "dr" {
  for_each                        = local.pilot
  region                          = var.dr_region
  identifier                      = "${local.prefix}-dr"
  replicate_source_db             = aws_rds_cluster.cell[0].arn
  instance_class                  = local.database_class
  storage_type                    = "gp3"
  iops                            = var.database_iops
  db_subnet_group_name            = aws_db_subnet_group.dr[each.key].name
  vpc_security_group_ids          = [aws_security_group.dr_database[each.key].id]
  parameter_group_name            = aws_db_parameter_group.dr[each.key].name
  storage_encrypted               = true
  kms_key_id                      = aws_kms_key.dr[each.key].arn
  backup_retention_period         = var.backup_retention_days
  copy_tags_to_snapshot           = true
  deletion_protection             = true
  final_snapshot_identifier       = "${local.prefix}-dr-final"
  performance_insights_enabled    = true
  performance_insights_kms_key_id = aws_kms_key.dr[each.key].arn
  auto_minor_version_upgrade      = false
  apply_immediately               = false
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_cloudwatch_metric_alarm" "replica_lag" {
  for_each            = local.pilot
  region              = var.dr_region
  alarm_name          = "${local.prefix}-dr-replica-lag"
  alarm_description   = "The DR replica is more than the RPO behind (ADR-0040)."
  namespace           = "AWS/RDS"
  metric_name         = "ReplicaLag"
  dimensions          = { DBInstanceIdentifier = aws_db_instance.dr[each.key].identifier }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = local.replica_lag_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = var.replica_lag_alarm_actions
}

# The control plane, without nodes: its role is the home cluster's (IAM is
# global), its secrets encrypted with the DR cluster key.

resource "aws_cloudwatch_log_group" "dr_cluster" {
  for_each          = local.pilot
  region            = var.dr_region
  name              = "/aws/eks/${local.prefix}/cluster"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.dr_cluster[each.key].arn
}

resource "aws_eks_cluster" "dr" {
  for_each = local.pilot
  region   = var.dr_region
  name     = local.prefix
  version  = var.kubernetes_version
  role_arn = aws_iam_role.cluster.arn
  vpc_config {
    subnet_ids              = module.dr_network[each.key].private_subnet_ids
    endpoint_private_access = true
    endpoint_public_access  = length(var.public_api_cidrs) > 0
    public_access_cidrs     = length(var.public_api_cidrs) > 0 ? var.public_api_cidrs : null
  }
  encryption_config {
    resources = ["secrets"]
    provider {
      key_arn = aws_kms_key.dr_cluster[each.key].arn
    }
  }
  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = false
  }
  enabled_cluster_log_types = ["api", "audit", "authenticator", "controllerManager", "scheduler"]
  depends_on                = [aws_iam_role_policy_attachment.cluster, aws_cloudwatch_log_group.dr_cluster]
}

resource "aws_eks_access_entry" "dr_admin" {
  for_each      = var.pilot_light ? toset(var.admin_role_arns) : toset([])
  region        = var.dr_region
  cluster_name  = aws_eks_cluster.dr["dr"].name
  principal_arn = each.value
}

resource "aws_eks_access_policy_association" "dr_admin" {
  for_each      = aws_eks_access_entry.dr_admin
  region        = var.dr_region
  cluster_name  = aws_eks_cluster.dr["dr"].name
  principal_arn = each.value.principal_arn
  policy_arn    = "arn:${data.aws_partition.current.partition}:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
  access_scope {
    type = "cluster"
  }
}

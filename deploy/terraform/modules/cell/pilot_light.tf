# A production cell's pilot light in its disaster-recovery region (ADR-0040,
# T41b2, T41b3): a cross-region read replica of the database, a VPC with
# its endpoints, an EKS control plane with a node group at zero, the
# cluster key's counterpart, the field key replicated (a multi-Region key,
# so tenant keys wrapped at home unwrap there) and the secrets replicated;
# the workloads' roles there trust the DR cluster alone
# (pilot_light_workloads.tf). On a region failure the runbook promotes the
# replica, scales the node group up and bootstraps the cell's Argo CD
# there, which syncs the cell. Whether a Multi-AZ DB cluster accepts a
# cross-region replica is to be confirmed on AWS staging before the pilot:
# the replica is one resource to replace if it does not.

variable "pilot_light" {
  description = "Keep a pilot light in dr_region (production cells); staging has the backup copies only."
  type        = bool
  default     = false
  validation {
    condition     = !var.pilot_light || var.dr_region != null
    error_message = "a pilot light needs a dr_region"
  }
}

variable "restore_drill_dr" {
  description = "Run the dr-copies restore drill's verification in dr_region: a VPC for the restored side cluster and a Fargate cluster for the verification (staging's drill cell, ADR-0040, T41e2)."
  type        = bool
  default     = false
  validation {
    condition     = !var.restore_drill_dr || var.dr_region != null
    error_message = "the restore drill's DR side needs a dr_region"
  }
}

variable "dr_cidr" {
  description = "The DR VPC's /16, used by no other cell or environment; required with pilot_light or restore_drill_dr."
  type        = string
  default     = null
  validation {
    condition     = !(var.pilot_light || var.restore_drill_dr) || var.dr_cidr != null
    error_message = "a DR VPC (pilot_light, restore_drill_dr) needs a dr_cidr"
  }
}

variable "dr_availability_zones" {
  description = "Three availability zones of dr_region; required with pilot_light or restore_drill_dr."
  type        = list(string)
  default     = []
  validation {
    condition     = !(var.pilot_light || var.restore_drill_dr) || length(var.dr_availability_zones) == 3
    error_message = "a DR VPC (pilot_light, restore_drill_dr) needs three availability zones of dr_region"
  }
}

variable "replica_lag_alarm_actions" {
  description = "Where the replica-lag alarm notifies (SNS topic ARNs of dr_region); required with pilot_light."
  type        = list(string)
  default     = []
  validation {
    # An alarm nobody hears is no RPO guard: fail closed.
    condition     = !var.pilot_light || length(var.replica_lag_alarm_actions) > 0
    error_message = "a pilot light's replica-lag alarm needs somewhere to notify"
  }
}

locals {
  pilot = var.pilot_light ? toset(["dr"]) : toset([])
  # The DR VPC, its database subnet group and security group: the pilot
  # light's replica lives there, and the restore drill restores the copies
  # there and verifies them from inside it (T41e2).
  dr_vpc = var.pilot_light || var.restore_drill_dr ? toset(["dr"]) : toset([])
  # The RPO: the replica may be this far behind (ADR-0040).
  replica_lag_seconds = 60
}

module "dr_network" {
  source             = "../network"
  for_each           = local.dr_vpc
  region             = var.dr_region
  name               = "${local.prefix}-dr"
  cidr               = var.dr_cidr
  availability_zones = var.dr_availability_zones
  # The drill's VPC carries a drill's traffic only: one NAT gateway is enough.
  zone_redundant = var.pilot_light
}

resource "aws_kms_key" "dr_cluster" {
  for_each                = local.pilot
  region                  = var.dr_region
  description             = "${local.prefix}: Kubernetes secrets and logs in the DR region (ADR-0040)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
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
      # The DR nodes' volumes, as the home cluster key's (kms.tf).
    ], local.autoscaling_volumes)
  })
  lifecycle {
    prevent_destroy = true
  }
}

# The field key's replica: the same key material and id, so the server
# unwraps the tenant keys there; its policy is the primary's but for the
# server's role, which is the DR server's (kms.tf field_key_policies).
resource "aws_kms_replica_key" "fields" {
  for_each                = local.pilot
  region                  = var.dr_region
  description             = "${local.prefix}: field encryption master, DR replica (ADR-0019, ADR-0040)"
  primary_key_arn         = aws_kms_key.fields.arn
  deletion_window_in_days = 30
  policy                  = local.field_key_policies[each.key]
  lifecycle {
    prevent_destroy = true
  }
}

# The database's replica, and the drill's side clusters (T41e2).

resource "aws_db_subnet_group" "dr" {
  for_each   = local.dr_vpc
  region     = var.dr_region
  name       = "${local.prefix}-dr"
  subnet_ids = module.dr_network[each.key].private_subnet_ids
}

resource "aws_security_group" "dr_database" {
  for_each    = local.dr_vpc
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
  for_each                = local.pilot
  region                  = var.dr_region
  identifier              = "${local.prefix}-dr"
  replicate_source_db     = aws_rds_cluster.cell[0].arn
  instance_class          = local.database_class
  storage_type            = "gp3"
  iops                    = var.database_iops
  db_subnet_group_name    = aws_db_subnet_group.dr[each.key].name
  vpc_security_group_ids  = [aws_security_group.dr_database[each.key].id]
  parameter_group_name    = aws_db_parameter_group.dr[each.key].name
  storage_encrypted       = true
  kms_key_id              = aws_kms_key.dr[each.key].arn
  backup_retention_period = var.backup_retention_days
  copy_tags_to_snapshot   = true
  deletion_protection     = true
  # Refused for a replica's deletion, which deletion_protection and
  # prevent_destroy forbid anyway; right once the replica is promoted.
  final_snapshot_identifier       = "${local.prefix}-dr-final"
  performance_insights_enabled    = true
  performance_insights_kms_key_id = aws_kms_key.dr[each.key].arn
  # The replica follows the cluster's engine version (a replica may not
  # run older than its source): the runbook upgrades it first.
  auto_minor_version_upgrade = false
  apply_immediately          = false
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

# The control plane: its role is the home cluster's (IAM is global), its
# secrets encrypted with the DR cluster key.

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

# The nodes, at zero until a failover (T41b3): the home cluster's node role
# (IAM is global) and instance types, its launch template's settings with
# the DR cluster key. The runbook scales the group to the cell's size
# (node_scaling) with the EKS API, since Terraform cannot plan while the
# home region is down; Terraform leaves the count alone afterwards.

resource "aws_launch_template" "dr_nodes" {
  for_each    = local.pilot
  region      = var.dr_region
  name_prefix = "${local.prefix}-dr-nodes-"
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size           = 50
      volume_type           = "gp3"
      encrypted             = true
      kms_key_id            = aws_kms_key.dr_cluster[each.key].arn
      delete_on_termination = true
    }
  }
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    http_endpoint               = "enabled"
  }
}

resource "aws_eks_node_group" "dr" {
  for_each        = local.pilot
  region          = var.dr_region
  cluster_name    = aws_eks_cluster.dr[each.key].name
  node_group_name = "${local.prefix}-general"
  node_role_arn   = aws_iam_role.nodes.arn
  subnet_ids      = module.dr_network[each.key].private_subnet_ids
  instance_types  = var.node_instance_types
  ami_type        = aws_eks_node_group.cell.ami_type
  launch_template {
    id      = aws_launch_template.dr_nodes[each.key].id
    version = aws_launch_template.dr_nodes[each.key].latest_version
  }
  scaling_config {
    min_size     = 0
    desired_size = 0
    max_size     = var.node_scaling.max
  }
  update_config {
    max_unavailable = 1
  }
  depends_on = [aws_iam_role_policy_attachment.nodes]
  lifecycle {
    ignore_changes = [scaling_config[0].desired_size, scaling_config[0].min_size]
  }
}

# The VPC CNI with its network policy agent, as at home, so the charts'
# default deny holds from the first pod. A DaemonSet, it is healthy with
# no nodes. The EBS CSI driver is not created here: its controller cannot
# become ready without nodes, so EKS would leave the add-on degraded and
# the apply would fail; the runbook creates it after the scale-up, as its
# role (dr_workload ebs_csi).
resource "aws_eks_addon" "dr_vpc_cni" {
  for_each                    = local.pilot
  region                      = var.dr_region
  cluster_name                = aws_eks_cluster.dr[each.key].name
  addon_name                  = "vpc-cni"
  addon_version               = var.vpc_cni_addon_version
  configuration_values        = jsonencode({ enableNetworkPolicy = "true" })
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"
}

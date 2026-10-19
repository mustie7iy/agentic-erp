# The dr-copies restore drill's side in the disaster-recovery region
# (ADR-0040, T41e2). A copy is restored into the DR VPC, where staging has
# no cluster to run the verification from and the home VPC is not peered:
# scripts/restore-drill.sh registers a task definition of the cell's server
# image (`erp-server dr verify`), runs it once on this Fargate cluster
# beside the restored database, reads its report from the log group, and
# removes the task definition and the side cluster after the run. What
# stays is what costs nothing at rest: the cluster, the roles, the
# security group and the logs.

locals {
  drill = var.restore_drill_dr ? toset(["dr"]) : toset([])
}

resource "aws_ecs_cluster" "drill" {
  for_each = local.drill
  region   = var.dr_region
  name     = "${local.prefix}-restore-drill"
}

resource "aws_cloudwatch_log_group" "drill" {
  for_each          = local.drill
  region            = var.dr_region
  name              = "/ecs/${local.prefix}/restore-drill"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.dr[each.key].arn
}

# The verification reaches the restored database in the VPC, and the
# registry, logs and secrets through the network's endpoints; nothing else.
resource "aws_security_group" "drill" {
  for_each    = local.drill
  region      = var.dr_region
  name        = "${local.prefix}-restore-drill"
  description = "The restore drill's verification task"
  vpc_id      = module.dr_network[each.key].vpc_id
  egress {
    description = "PostgreSQL in the DR VPC"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [module.dr_network[each.key].cidr]
  }
  egress {
    description = "HTTPS: the registry, logs and secrets"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# The task's execution role: pull the image (AWS's managed baseline, as for
# backups) and read the side cluster's RDS-managed master secret into the
# task's PGPASSWORD. The task itself has no role: the verification reads
# the database only.
resource "aws_iam_role" "drill" {
  for_each = local.drill
  name     = "${local.prefix}-restore-drill"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "drill" {
  for_each   = local.drill
  role       = aws_iam_role.drill[each.key].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "drill" {
  for_each = local.drill
  role     = aws_iam_role.drill[each.key].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = "secretsmanager:GetSecretValue"
      # The drill's side clusters only: RDS names a cluster's managed
      # secret after it, and the drill names the clusters <cell>-drill-*.
      Resource = "arn:${data.aws_partition.current.partition}:secretsmanager:${var.dr_region}:${data.aws_caller_identity.current.account_id}:secret:rds!cluster-*"
      Condition = {
        StringLike = { "secretsmanager:ResourceTag/aws:rds:primaryDBClusterArn" = "arn:${data.aws_partition.current.partition}:rds:${var.dr_region}:${data.aws_caller_identity.current.account_id}:cluster:${local.prefix}-drill-*" }
      }
    }]
  })
}

output "restore_drill" {
  description = "The dr-copies drill's side in dr_region (scripts/restore-drill.sh RESTORE_DR_TASK); null without restore_drill_dr."
  value = var.restore_drill_dr ? {
    region             = var.dr_region
    cluster            = aws_ecs_cluster.drill["dr"].name
    subnets            = module.dr_network["dr"].private_subnet_ids
    security_group     = aws_security_group.drill["dr"].id
    execution_role_arn = aws_iam_role.drill["dr"].arn
    log_group          = aws_cloudwatch_log_group.drill["dr"].name
    db_subnet_group    = aws_db_subnet_group.dr["dr"].name
    db_security_group  = aws_security_group.dr_database["dr"].id
    vault              = aws_backup_vault.dr["dr"].name
  } : null
}

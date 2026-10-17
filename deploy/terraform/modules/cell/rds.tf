# The cell's database: RDS for PostgreSQL 17, behind PgBouncer on the
# cluster (T40c) because RDS Proxy pins sessions on SET LOCAL (ADR-0003).
# In the production shape (zone_redundant) it is a Multi-AZ DB cluster, a
# writer and two readable standbys in three zones; dev and test run a
# single-AZ instance on small gp3 storage. Either way it requires TLS 1.3
# from every client (T38), is encrypted with the cell's data key, keeps two
# weeks of point-in-time recovery (T41 builds on it), and cannot be deleted
# by accident. The master password is RDS-managed in Secrets Manager; the
# platform's roles are created by its migrations.

data "aws_rds_engine_version" "postgres" {
  engine  = "postgres"
  version = "17"
  latest  = true
}

locals {
  database_class      = coalesce(var.database_instance_class, var.zone_redundant ? "db.m6gd.large" : "db.t4g.medium")
  database_storage_gb = coalesce(var.database_storage_gb, var.zone_redundant ? 400 : 20)
  database_master_secret_arn = (var.zone_redundant
    ? one(flatten(aws_rds_cluster.cell[*].master_user_secret[*].secret_arn))
  : one(flatten(aws_db_instance.cell[*].master_user_secret[*].secret_arn)))
  # Both shapes' parameter groups set these.
  database_parameters = {
    "rds.force_ssl"              = "1"
    "ssl_min_protocol_version"   = "TLSv1.3"
    "log_min_duration_statement" = "1000"
  }
}

resource "aws_db_subnet_group" "cell" {
  name       = local.prefix
  subnet_ids = module.network.private_subnet_ids
}

resource "aws_security_group" "database" {
  name        = "${local.prefix}-database"
  description = "PostgreSQL from inside the cell"
  vpc_id      = module.network.vpc_id
  ingress {
    description = "PostgreSQL from the cell VPC"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [module.network.cidr]
  }
}

resource "aws_cloudwatch_log_group" "database" {
  name              = var.zone_redundant ? "/aws/rds/cluster/${local.prefix}/postgresql" : "/aws/rds/instance/${local.prefix}/postgresql"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.cluster.arn
}

# The production shape.

resource "aws_rds_cluster_parameter_group" "cell" {
  count  = var.zone_redundant ? 1 : 0
  name   = "${local.prefix}-postgres17"
  family = "postgres17"
  dynamic "parameter" {
    for_each = local.database_parameters
    content {
      name  = parameter.key
      value = parameter.value
    }
  }
}

resource "aws_rds_cluster" "cell" {
  count                           = var.zone_redundant ? 1 : 0
  cluster_identifier              = local.prefix
  engine                          = "postgres"
  engine_version                  = data.aws_rds_engine_version.postgres.version_actual
  db_cluster_instance_class       = local.database_class
  storage_type                    = "gp3"
  allocated_storage               = local.database_storage_gb
  iops                            = var.database_iops
  database_name                   = "erp"
  master_username                 = "erp_admin"
  manage_master_user_password     = true
  master_user_secret_kms_key_id   = aws_kms_key.data.arn
  db_subnet_group_name            = aws_db_subnet_group.cell.name
  vpc_security_group_ids          = [aws_security_group.database.id]
  db_cluster_parameter_group_name = aws_rds_cluster_parameter_group.cell[0].name
  storage_encrypted               = true
  kms_key_id                      = aws_kms_key.data.arn
  backup_retention_period         = 14
  preferred_backup_window         = "02:00-03:00"
  preferred_maintenance_window    = "sun:03:30-sun:04:30"
  copy_tags_to_snapshot           = true
  deletion_protection             = true
  final_snapshot_identifier       = "${local.prefix}-final"
  enabled_cloudwatch_logs_exports = ["postgresql"]
  performance_insights_enabled    = true
  performance_insights_kms_key_id = aws_kms_key.data.arn
  apply_immediately               = false
  depends_on                      = [aws_cloudwatch_log_group.database]
  lifecycle {
    prevent_destroy = true
  }
}

# Dev and test.

resource "aws_db_parameter_group" "cell" {
  count  = var.zone_redundant ? 0 : 1
  name   = "${local.prefix}-postgres17"
  family = "postgres17"
  dynamic "parameter" {
    for_each = local.database_parameters
    content {
      name  = parameter.key
      value = parameter.value
    }
  }
}

resource "aws_db_instance" "cell" {
  count          = var.zone_redundant ? 0 : 1
  identifier     = local.prefix
  engine         = "postgres"
  engine_version = data.aws_rds_engine_version.postgres.version_actual
  instance_class = local.database_class
  multi_az       = false
  # The zone of the NAT gateway and the endpoints: one zone's outage, not
  # any of three, takes a dev or test cell down.
  availability_zone               = var.availability_zones[0]
  storage_type                    = "gp3"
  allocated_storage               = local.database_storage_gb
  max_allocated_storage           = max(200, local.database_storage_gb)
  db_name                         = "erp"
  username                        = "erp_admin"
  manage_master_user_password     = true
  master_user_secret_kms_key_id   = aws_kms_key.data.arn
  db_subnet_group_name            = aws_db_subnet_group.cell.name
  vpc_security_group_ids          = [aws_security_group.database.id]
  parameter_group_name            = aws_db_parameter_group.cell[0].name
  storage_encrypted               = true
  kms_key_id                      = aws_kms_key.data.arn
  backup_retention_period         = 14
  backup_window                   = "02:00-03:00"
  maintenance_window              = "sun:03:30-sun:04:30"
  copy_tags_to_snapshot           = true
  deletion_protection             = true
  final_snapshot_identifier       = "${local.prefix}-final"
  enabled_cloudwatch_logs_exports = ["postgresql"]
  performance_insights_enabled    = true
  performance_insights_kms_key_id = aws_kms_key.data.arn
  auto_minor_version_upgrade      = true
  apply_immediately               = false
  depends_on                      = [aws_cloudwatch_log_group.database]
  lifecycle {
    prevent_destroy = true
  }
}

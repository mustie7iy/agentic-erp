# The cell's database: RDS for PostgreSQL 17 as a Multi-AZ DB cluster (a
# writer and two readable standbys in three zones), behind PgBouncer on the
# cluster (T40c) because RDS Proxy pins sessions on SET LOCAL (ADR-0003).
# It requires TLS 1.3 from every client (T38), is encrypted with the cell's
# data key, keeps two weeks of point-in-time recovery (T41 builds on it),
# and cannot be deleted by accident. The master password is RDS-managed in
# Secrets Manager; the platform's roles are created by its migrations.

data "aws_rds_engine_version" "postgres" {
  engine  = "postgres"
  version = "17"
  latest  = true
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

resource "aws_rds_cluster_parameter_group" "cell" {
  name   = "${local.prefix}-postgres17"
  family = "postgres17"
  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }
  parameter {
    name  = "ssl_min_protocol_version"
    value = "TLSv1.3"
  }
  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
  }
}

resource "aws_cloudwatch_log_group" "database" {
  name              = "/aws/rds/cluster/${local.prefix}/postgresql"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.cluster.arn
}

resource "aws_rds_cluster" "cell" {
  cluster_identifier              = local.prefix
  engine                          = "postgres"
  engine_version                  = data.aws_rds_engine_version.postgres.version_actual
  db_cluster_instance_class       = var.database_instance_class
  storage_type                    = "gp3"
  allocated_storage               = var.database_storage_gb
  iops                            = var.database_iops
  database_name                   = "erp"
  master_username                 = "erp_admin"
  manage_master_user_password     = true
  master_user_secret_kms_key_id   = aws_kms_key.data.arn
  db_subnet_group_name            = aws_db_subnet_group.cell.name
  vpc_security_group_ids          = [aws_security_group.database.id]
  db_cluster_parameter_group_name = aws_rds_cluster_parameter_group.cell.name
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

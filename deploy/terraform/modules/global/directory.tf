# The global directory (internal/directory, ADR-0022): its own PostgreSQL 17
# instance, Multi-AZ with a standby in another zone (single-AZ in dev and
# test), written by the cells and read by the router. TLS 1.3 only, as in
# the cells (T38).

data "aws_rds_engine_version" "postgres" {
  engine  = "postgres"
  version = "17"
  latest  = true
}

resource "aws_db_subnet_group" "directory" {
  name       = "${local.prefix}-directory"
  subnet_ids = var.private_subnet_ids
}

resource "aws_security_group" "directory" {
  name        = "${local.prefix}-directory"
  description = "PostgreSQL of the global directory"
  vpc_id      = var.vpc_id
  ingress {
    description = "PostgreSQL from the home cell VPC"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }
}

resource "aws_db_parameter_group" "directory" {
  name   = "${local.prefix}-directory-postgres17"
  family = "postgres17"
  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }
  parameter {
    name  = "ssl_min_protocol_version"
    value = "TLSv1.3"
  }
}

resource "aws_cloudwatch_log_group" "directory" {
  name              = "/aws/rds/instance/${local.prefix}-directory/postgresql"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.directory.arn
}

resource "aws_db_instance" "directory" {
  identifier     = "${local.prefix}-directory"
  engine         = "postgres"
  engine_version = data.aws_rds_engine_version.postgres.version_actual
  instance_class = var.directory_instance_class
  multi_az       = var.directory_multi_az
  # In one zone, the first of the home cell's private subnets, where its
  # NAT gateway and endpoints are; AWS refuses a zone with Multi-AZ.
  availability_zone               = var.directory_multi_az ? null : var.directory_zone
  storage_type                    = "gp3"
  allocated_storage               = 20
  max_allocated_storage           = 200
  db_name                         = "directory"
  username                        = "erp_admin"
  manage_master_user_password     = true
  master_user_secret_kms_key_id   = aws_kms_key.directory.arn
  db_subnet_group_name            = aws_db_subnet_group.directory.name
  vpc_security_group_ids          = [aws_security_group.directory.id]
  parameter_group_name            = aws_db_parameter_group.directory.name
  storage_encrypted               = true
  kms_key_id                      = aws_kms_key.directory.arn
  backup_retention_period         = 14
  copy_tags_to_snapshot           = true
  deletion_protection             = true
  final_snapshot_identifier       = "${local.prefix}-directory-final"
  enabled_cloudwatch_logs_exports = ["postgresql"]
  performance_insights_enabled    = true
  performance_insights_kms_key_id = aws_kms_key.directory.arn
  auto_minor_version_upgrade      = true
  apply_immediately               = false
  depends_on                      = [aws_cloudwatch_log_group.directory]
  lifecycle {
    prevent_destroy = true
  }
}

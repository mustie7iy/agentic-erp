# Resources that leave data unencrypted at rest, one of each kind the
# encryption policy (deploy/policy/encryption.rego) covers. scripts/
# deploy-checks.sh runs conftest on this directory and fails unless every
# one of them is refused. Never applied.

resource "aws_s3_bucket" "plain" {
  bucket = "erp-policy-fixture-plain"
}

resource "aws_s3_bucket" "aes" {
  bucket = "erp-policy-fixture-aes"
}

resource "aws_s3_bucket_server_side_encryption_configuration" "aes" {
  bucket = aws_s3_bucket.aes.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "aes" {
  bucket                  = aws_s3_bucket.aes.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_kms_key" "unrotated" {
  description = "a symmetric key without rotation"
}

resource "aws_ecr_repository" "aes" {
  name = "erp-policy-fixture"
  encryption_configuration {
    encryption_type = "AES256"
  }
}

resource "aws_ecr_repository_creation_template" "aes" {
  prefix      = "ROOT"
  applied_for = ["REPLICATION"]
  encryption_configuration {
    encryption_type = "AES256"
  }
}

resource "aws_ebs_volume" "plain" {
  availability_zone = "eu-central-1a"
  size              = 1
}

resource "aws_db_instance" "plain" {
  engine         = "postgres"
  instance_class = "db.t4g.micro"
}

resource "aws_rds_cluster" "plain" {
  engine = "aurora-postgresql"
}

resource "aws_eks_cluster" "plain" {
  name     = "erp-policy-fixture"
  role_arn = "arn:aws:iam::111111111111:role/eks"
  vpc_config {
    subnet_ids = ["subnet-1"]
  }
}

resource "aws_dynamodb_table" "plain" {
  name     = "erp-policy-fixture"
  hash_key = "id"
  attribute {
    name = "id"
    type = "S"
  }
}

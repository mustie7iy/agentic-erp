# A VPC whose workloads would reach S3, KMS and STS through the internet
# rather than PrivateLink (deploy/policy/endpoints.rego). scripts/deploy-
# checks.sh fails unless each missing endpoint is refused. Never applied.

resource "aws_vpc" "cell" {
  cidr_block = "10.0.0.0/16"
}

resource "aws_vpc_endpoint" "ecr" {
  vpc_id       = aws_vpc.cell.id
  service_name = "com.amazonaws.eu-central-1.ecr.api"
}

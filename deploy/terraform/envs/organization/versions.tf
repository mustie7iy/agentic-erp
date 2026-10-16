terraform {
  required_version = ">= 1.16.0, < 2.0.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.68"
    }
  }
  # Partial: bucket and region come from `terraform init -backend-config`
  # (the state bucket of envs/bootstrap). use_lockfile is S3's own locking.
  backend "s3" {
    key          = "organization/terraform.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region              = var.region
  allowed_account_ids = [var.account_id]
  default_tags {
    tags = {
      system     = "agentic-erp"
      managed_by = "terraform"
      root       = "envs/organization"
    }
  }
}

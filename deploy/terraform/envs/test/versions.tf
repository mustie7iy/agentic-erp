terraform {
  required_version = ">= 1.16.0, < 2.0.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.68"
    }
  }
  # Partial: bucket, region and kms_key_id come from `terraform init
  # -backend-config` (the state bucket of envs/bootstrap, ADR-0035).
  backend "s3" {
    key          = "test/terraform.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region              = var.region
  allowed_account_ids = [var.account_id]
  default_tags {
    tags = {
      system      = "agentic-erp"
      managed_by  = "terraform"
      root        = "envs/test"
      environment = "test"
    }
  }
}

# The tooling account, where the root CA signs the environment's
# subordinate.
provider "aws" {
  alias               = "tooling"
  region              = var.tooling_region
  allowed_account_ids = [var.tooling_account_id]
  assume_role {
    role_arn = var.tooling_role_arn
  }
  default_tags {
    tags = {
      system      = "agentic-erp"
      managed_by  = "terraform"
      root        = "envs/test"
      environment = "test"
    }
  }
}

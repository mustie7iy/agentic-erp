# The tooling root wired together against a mock provider: the roles, the
# key that names them and the policies that name the key form no cycle, and
# the outputs are what the release environment needs.
mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_data "aws_kms_public_key" {
    defaults = { public_key_pem = "-----BEGIN PUBLIC KEY-----\nMFkw\n-----END PUBLIC KEY-----\n" }
  }
  mock_resource "aws_kms_key" {
    defaults = { arn = "arn:aws:kms:eu-central-1:111111111111:key/0d5c2b1e-0000-4000-8000-000000000000" }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::111111111111:role/mock" }
  }
  mock_resource "aws_ecr_repository" {
    defaults = { arn = "arn:aws:ecr:eu-central-1:111111111111:repository/mock" }
  }
  mock_resource "aws_acmpca_certificate_authority" {
    defaults = { arn = "arn:aws:acm-pca:eu-central-1:111111111111:certificate-authority/00000000-0000-0000-0000-000000000000" }
  }
  mock_resource "aws_iam_openid_connect_provider" {
    defaults = { arn = "arn:aws:iam::111111111111:oidc-provider/token.actions.githubusercontent.com" }
  }
}

override_resource {
  target = module.roles.aws_iam_role.release
  values = { arn = "arn:aws:iam::111111111111:role/erp-release" }
}

override_resource {
  target = module.roles.aws_iam_role.delivery
  values = { arn = "arn:aws:iam::111111111111:role/erp-delivery" }
}

variables {
  account_id              = "111111111111"
  region                  = "eu-central-1"
  admin_role_arns         = ["arn:aws:iam::111111111111:role/platform-admin"]
  environment_account_ids = ["222222222222", "333333333333", "444444444444", "555555555555"]
  replication_regions     = ["eu-west-1"]
  ca_organization         = "agentic-erp"
  ca_country              = "DE"
}

run "the_tooling_account_plans_and_wires_its_parts" {
  command = apply

  assert {
    condition     = startswith(output.cosign_public_key, "-----BEGIN PUBLIC KEY-----")
    error_message = "the admission policy's key is the release key's public half"
  }
  assert {
    condition     = output.cosign_key == "awskms:///arn:aws:kms:eu-central-1:111111111111:key/0d5c2b1e-0000-4000-8000-000000000000"
    error_message = "ERP_COSIGN_KEY is the signing key's awskms reference"
  }
  assert {
    condition     = length(output.repository_urls) == 7
    error_message = "one repository per binary and per mirrored upstream image"
  }
  assert {
    condition     = [for s in jsondecode(module.signing_key.policy).Statement : s.Principal.AWS if s.Sid == "Sign"][0] == ["arn:aws:iam::111111111111:role/erp-release"]
    error_message = "the release role, not delivery, signs"
  }
  assert {
    condition     = contains([for s in jsondecode(module.signing_key.policy).Statement : s.Principal.AWS if s.Sid == "Verify"][0], "arn:aws:iam::111111111111:role/erp-delivery")
    error_message = "delivery reads the public key"
  }
  assert {
    condition     = output.region == "eu-central-1"
    error_message = "the release environment's region"
  }
}

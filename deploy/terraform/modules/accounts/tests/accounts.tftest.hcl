mock_provider "aws" {
  mock_resource "aws_organizations_organizational_unit" {
    defaults = { id = "ou-abcd-12345678" }
  }
}

variables {
  parent_id = "r-abcd"
  accounts = {
    tooling    = { email = "aws+tooling@example.com" }
    dev        = { email = "aws+dev@example.com" }
    test       = { email = "aws+test@example.com" }
    staging    = { email = "aws+staging@example.com" }
    production = { email = "aws+production@example.com" }
  }
}

run "an_account_per_environment_plus_tooling" {
  command = apply

  assert {
    condition     = length(aws_organizations_account.this) == 5
    error_message = "want five accounts"
  }
  assert {
    condition     = aws_organizations_account.this["production"].name == "erp-production" && !aws_organizations_account.this["production"].close_on_deletion
    error_message = "the production account is named erp-production and never closed by Terraform"
  }
}

run "an_environment_missing_is_refused" {
  command = plan
  variables {
    accounts = {
      tooling = { email = "aws+tooling@example.com" }
      dev     = { email = "aws+dev@example.com" }
    }
  }
  expect_failures = [var.accounts]
}

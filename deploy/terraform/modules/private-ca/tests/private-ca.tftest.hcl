mock_provider "aws" {
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_resource "aws_acmpca_certificate_authority" {
    defaults = { arn = "arn:aws:acm-pca:eu-central-1:111111111111:certificate-authority/00000000-0000-0000-0000-000000000000" }
  }
}

variables {
  organization = "agentic-erp"
  country      = "DE"
}

run "a_self_signed_root_installed_and_kept" {
  command = apply

  assert {
    condition     = aws_acmpca_certificate_authority.root.type == "ROOT" && aws_acmpca_certificate_authority.root.usage_mode == "GENERAL_PURPOSE"
    error_message = "a general-purpose root"
  }
  assert {
    condition     = aws_acmpca_certificate.root.template_arn == "arn:aws:acm-pca:::template/RootCACertificate/V1"
    error_message = "the root certificate is issued with the root template"
  }
  assert {
    condition     = aws_acmpca_certificate_authority.root.certificate_authority_configuration[0].key_algorithm == "EC_secp384r1"
    error_message = "an EC P-384 root key"
  }
}

run "a_country_is_two_letters" {
  command = plan
  variables {
    country = "Germany"
  }
  expect_failures = [var.country]
}

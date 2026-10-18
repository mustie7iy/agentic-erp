mock_provider "aws" {
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_resource "aws_acmpca_certificate_authority" {
    defaults = { arn = "arn:aws:acm-pca:eu-central-1:222222222222:certificate-authority/2222" }
  }
}

mock_provider "aws" {
  alias = "tooling"
}

variables {
  environment      = "dev"
  organization     = "agentic-erp"
  country          = "DE"
  root_ca_arn      = "arn:aws:acm-pca:eu-central-1:111111111111:certificate-authority/1111"
  root_certificate = "-----BEGIN CERTIFICATE-----\nroot\n-----END CERTIFICATE-----"
}

run "the_root_signs_a_subordinate_that_issues_end_entities_only" {
  command = apply
  providers = {
    aws         = aws
    aws.tooling = aws.tooling
  }

  assert {
    condition     = aws_acmpca_certificate_authority.subordinate.type == "SUBORDINATE" && aws_acmpca_certificate_authority.subordinate.usage_mode == "GENERAL_PURPOSE"
    error_message = "a general-purpose subordinate CA"
  }
  assert {
    condition     = aws_acmpca_certificate.subordinate.certificate_authority_arn == var.root_ca_arn
    error_message = "the root in tooling signs it"
  }
  assert {
    condition     = endswith(aws_acmpca_certificate.subordinate.template_arn, "SubordinateCACertificate_PathLen0/V1")
    error_message = "path length 0: it cannot sign further CAs"
  }
  assert {
    condition     = aws_acmpca_certificate_authority_certificate.subordinate.certificate_chain == var.root_certificate
    error_message = "installed with the root as its chain"
  }
}

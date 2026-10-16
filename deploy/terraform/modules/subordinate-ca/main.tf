# An environment's subordinate certificate authority (ADR-0035, T38): it
# lives in the environment's account, and the root CA in tooling signs its
# certificate (path length 0: it issues end-entity certificates only).
# cert-manager's aws-privateca-issuer in each of the environment's cells
# issues from it. General-purpose mode, as the product owner decided.
#
# Two providers: the default one is the environment's account, `tooling`
# the tooling account, where the root signs.

terraform {
  required_version = ">= 1.16.0, < 2.0.0"
  required_providers {
    aws = {
      source                = "hashicorp/aws"
      version               = "~> 6.68"
      configuration_aliases = [aws.tooling]
    }
  }
}

variable "environment" {
  description = "The environment (dev, test, staging, production)."
  type        = string
  validation {
    condition     = contains(["dev", "test", "staging", "production"], var.environment)
    error_message = "one of dev, test, staging, production"
  }
}

variable "organization" {
  description = "The organization in the subject."
  type        = string
}

variable "country" {
  description = "The two-letter country of the subject."
  type        = string
  validation {
    condition     = can(regex("^[A-Z]{2}$", var.country))
    error_message = "a two-letter country code, e.g. DE"
  }
}

variable "root_ca_arn" {
  description = "The root CA in tooling (envs/tooling's root_ca_arn)."
  type        = string
}

variable "root_certificate" {
  description = "The root certificate (PEM), the chain of the subordinate's certificate."
  type        = string
}

variable "validity_years" {
  description = "How long the subordinate's certificate is valid (less than the root's)."
  type        = number
  default     = 5
}

variable "key_storage_security_standard" {
  description = "FIPS level of the CA's key storage; some regions offer only FIPS_140_2_LEVEL_2_OR_HIGHER."
  type        = string
  default     = "FIPS_140_2_LEVEL_3_OR_HIGHER"
}

data "aws_partition" "current" {}

resource "aws_acmpca_certificate_authority" "subordinate" {
  type       = "SUBORDINATE"
  usage_mode = "GENERAL_PURPOSE"
  certificate_authority_configuration {
    key_algorithm     = "EC_prime256v1"
    signing_algorithm = "SHA256WITHECDSA"
    subject {
      common_name  = "agentic-erp ${var.environment} CA"
      organization = var.organization
      country      = var.country
    }
  }
  key_storage_security_standard   = var.key_storage_security_standard
  permanent_deletion_time_in_days = 30
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_acmpca_certificate" "subordinate" {
  provider                    = aws.tooling
  certificate_authority_arn   = var.root_ca_arn
  certificate_signing_request = aws_acmpca_certificate_authority.subordinate.certificate_signing_request
  signing_algorithm           = "SHA384WITHECDSA"
  template_arn                = "arn:${data.aws_partition.current.partition}:acm-pca:::template/SubordinateCACertificate_PathLen0/V1"
  validity {
    type  = "YEARS"
    value = var.validity_years
  }
}

resource "aws_acmpca_certificate_authority_certificate" "subordinate" {
  certificate_authority_arn = aws_acmpca_certificate_authority.subordinate.arn
  certificate               = aws_acmpca_certificate.subordinate.certificate
  certificate_chain         = var.root_certificate
}

output "ca_arn" {
  description = "The environment's subordinate CA, for cert-manager's issuer."
  value       = aws_acmpca_certificate_authority.subordinate.arn
}

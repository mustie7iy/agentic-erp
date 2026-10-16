# The platform's root certificate authority in AWS Private CA (ADR-0035,
# from T38): it signs one subordinate CA per environment (T40b), from which
# cert-manager's aws-privateca-issuer issues the short-lived certificates
# of TLS inside a cell and of mTLS between services. The root signs nothing
# else, lives in the tooling account and is kept from deletion.

variable "common_name" {
  description = "The root's common name."
  type        = string
  default     = "agentic-erp root CA"
}

variable "organization" {
  description = "The organization in the root's subject."
  type        = string
}

variable "country" {
  description = "The two-letter country of the root's subject."
  type        = string
  validation {
    condition     = can(regex("^[A-Z]{2}$", var.country))
    error_message = "a two-letter country code, e.g. DE"
  }
}

variable "validity_years" {
  description = "How long the root certificate is valid."
  type        = number
  default     = 10
}

resource "aws_acmpca_certificate_authority" "root" {
  type       = "ROOT"
  usage_mode = "GENERAL_PURPOSE"
  certificate_authority_configuration {
    key_algorithm     = "EC_secp384r1"
    signing_algorithm = "SHA384WITHECDSA"
    subject {
      common_name  = var.common_name
      organization = var.organization
      country      = var.country
    }
  }
  key_storage_security_standard   = "FIPS_140_2_LEVEL_3_OR_HIGHER"
  permanent_deletion_time_in_days = 30
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_acmpca_certificate" "root" {
  certificate_authority_arn   = aws_acmpca_certificate_authority.root.arn
  certificate_signing_request = aws_acmpca_certificate_authority.root.certificate_signing_request
  signing_algorithm           = "SHA384WITHECDSA"
  template_arn                = "arn:${data.aws_partition.current.partition}:acm-pca:::template/RootCACertificate/V1"
  validity {
    type  = "YEARS"
    value = var.validity_years
  }
}

resource "aws_acmpca_certificate_authority_certificate" "root" {
  certificate_authority_arn = aws_acmpca_certificate_authority.root.arn
  certificate               = aws_acmpca_certificate.root.certificate
  certificate_chain         = aws_acmpca_certificate.root.certificate_chain
}

data "aws_partition" "current" {}

output "root_ca_arn" {
  description = "The root CA, which signs the environments' subordinate CAs."
  value       = aws_acmpca_certificate_authority.root.arn
}

output "root_certificate" {
  description = "The root certificate (PEM), the trust anchor of every cell."
  value       = aws_acmpca_certificate.root.certificate
}

# The tooling account (ADR-0035): what every environment trusts and pulls
# from. The release signing key and the roles GitHub Actions assumes
# (ADR-0032), the image registry replicated to the cells' regions, and the
# root certificate authority. Its outputs are the settings of the GitHub
# `release` environment (ERP_COSIGN_KEY, ERP_RELEASE_ROLE_ARN,
# ERP_AWS_REGION) and of delivery.

variable "account_id" {
  description = "The tooling account's id; any other account is refused."
  type        = string
}

variable "region" {
  description = "The tooling account's primary region (the registry's, the key's)."
  type        = string
}

variable "repository" {
  description = "The GitHub repository whose workflows may assume the roles."
  type        = string
  default     = "mustie7iy/agentic-erp"
}

variable "admin_role_arns" {
  description = "Roles that administer the signing key (the platform operators' role)."
  type        = list(string)
}

variable "verifier_role_arns" {
  description = "Further roles that read the signing key's public half (the clusters' admission policy, T40c)."
  type        = list(string)
  default     = []
}

variable "environment_account_ids" {
  description = "The environment accounts, which pull the images."
  type        = list(string)
}

variable "replication_regions" {
  description = "The cells' regions other than the primary, which receive replicas of the images."
  type        = list(string)
  default     = []
}

variable "ca_organization" {
  description = "The organization in the root CA's subject."
  type        = string
}

variable "ca_country" {
  description = "The country in the root CA's subject."
  type        = string
}

module "roles" {
  source          = "../../modules/release-roles"
  repository      = var.repository
  signing_key_arn = module.signing_key.key_arn
  repository_arns = values(module.ecr.repository_arns)
}

module "signing_key" {
  source             = "../../modules/signing-key"
  admin_role_arns    = var.admin_role_arns
  signer_role_arns   = [module.roles.release_role_arn]
  verifier_role_arns = concat([module.roles.delivery_role_arn], var.verifier_role_arns)
}

module "ecr" {
  source              = "../../modules/ecr"
  replication_regions = var.replication_regions
  pull_account_ids    = var.environment_account_ids
}

module "private_ca" {
  source       = "../../modules/private-ca"
  organization = var.ca_organization
  country      = var.ca_country
}

output "cosign_key" {
  description = "ERP_COSIGN_KEY of the release environment, and ERP_COSIGN_PUBLIC_KEY of delivery."
  value       = module.signing_key.cosign_key
}

output "region" {
  description = "ERP_AWS_REGION of the release environment: the signing key's and the registry's region."
  value       = var.region
}

output "release_role_arn" {
  description = "ERP_RELEASE_ROLE_ARN of the release environment."
  value       = module.roles.release_role_arn
}

output "delivery_role_arn" {
  description = "The role delivery assumes."
  value       = module.roles.delivery_role_arn
}

output "repository_urls" {
  description = "Where releases push, by binary."
  value       = module.ecr.repository_urls
}

output "root_ca_arn" {
  description = "The root CA the environments' subordinate CAs are signed by."
  value       = module.private_ca.root_ca_arn
}

output "root_certificate" {
  description = "The root certificate (PEM): the chain of the subordinate CAs and every cell's trust anchor."
  value       = module.private_ca.root_certificate
}

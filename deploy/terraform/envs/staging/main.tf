# The staging environment (ADR-0035): one cell, stg1, its global tier and
# the environment's subordinate CA, in the production shape, where every
# release passes its rollback drill before production.
# Regions, administrator roles and the CA's subject are settled at first
# apply, in a terraform.tfvars kept out of the repository.

variable "account_id" {
  description = "The staging account's id; any other account is refused."
  type        = string
}

variable "region" {
  description = "The region of the cell stg1."
  type        = string
}

variable "availability_zones" {
  description = "Three availability zones of the region."
  type        = list(string)
}

variable "admin_role_arns" {
  description = "Roles that administer the environment's keys and clusters; must include the role that applies."
  type        = list(string)
}

variable "public_api_cidrs" {
  description = "CIDRs allowed to reach the Kubernetes API publicly; empty keeps it private."
  type        = list(string)
  default     = []
}

variable "tooling_account_id" {
  description = "The tooling account, home of the root CA."
  type        = string
}

variable "tooling_region" {
  description = "The tooling account's primary region (envs/tooling's region)."
  type        = string
}

variable "tooling_role_arn" {
  description = "The role in tooling this apply assumes to have the root CA sign the subordinate."
  type        = string
}

variable "root_ca_arn" {
  description = "envs/tooling's root_ca_arn."
  type        = string
}

variable "root_certificate" {
  description = "The root certificate (PEM), envs/tooling's root_certificate."
  type        = string
}

variable "ca_organization" {
  description = "The organization in the subordinate CA's subject."
  type        = string
}

variable "ca_country" {
  description = "The country in the subordinate CA's subject."
  type        = string
}

variable "dr_region" {
  description = "The disaster-recovery region the cell's backups are copied to and its bucket replicates to (ADR-0040)."
  type        = string
}

variable "dr_availability_zones" {
  description = "Three availability zones of dr_region, for the restore drill's VPC there (ADR-0040)."
  type        = list(string)
}

module "environment" {
  source = "../../modules/environment"
  providers = {
    aws         = aws
    aws.tooling = aws.tooling
  }
  environment        = "staging"
  cell               = "stg1"
  cidr               = "10.48.0.0/16"
  availability_zones = var.availability_zones
  admin_role_arns    = var.admin_role_arns
  public_api_cidrs   = var.public_api_cidrs
  tooling_account_id = var.tooling_account_id
  root_ca_arn        = var.root_ca_arn
  root_certificate   = var.root_certificate
  ca_organization    = var.ca_organization
  ca_country         = var.ca_country
  dr_region          = var.dr_region
  # The dr-copies drill restores into, and verifies from, a VPC of the DR
  # region (T41e2).
  restore_drill_dr      = true
  dr_cidr               = "10.49.0.0/16"
  dr_availability_zones = var.dr_availability_zones
}

output "cell" {
  description = "The cell stg1: cluster, database, bucket, keys and workload roles (the erp-cell chart's values)."
  value       = module.environment.cell
}

output "global" {
  description = "The directory and the router's load balancer (the erp-global chart's values)."
  value       = module.environment.global
}

output "subordinate_ca_certificate" {
  description = "The environment's trust anchor (the charts' trust.caBundle, ERP_TLS_CA)."
  value       = module.environment.subordinate_ca_certificate
}

output "subordinate_ca_arn" {
  description = "The environment's subordinate CA (cert-manager's AWSPCAClusterIssuer)."
  value       = module.environment.subordinate_ca_arn
}

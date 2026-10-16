# The test environment (ADR-0035): one cell, test-1, its global tier and the
# environment's subordinate CA. Regions, administrator roles and the CA's
# subject are settled at first apply, in a terraform.tfvars kept out of the
# repository.

variable "account_id" {
  description = "The test account's id; any other account is refused."
  type        = string
}

variable "region" {
  description = "The region of the cell test-1."
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

module "subordinate_ca" {
  source = "../../modules/subordinate-ca"
  providers = {
    aws         = aws
    aws.tooling = aws.tooling
  }
  environment      = "test"
  organization     = var.ca_organization
  country          = var.ca_country
  root_ca_arn      = var.root_ca_arn
  root_certificate = var.root_certificate
}

module "cell" {
  source             = "../../modules/cell"
  name               = "test-1"
  cidr               = "10.32.0.0/16"
  availability_zones = var.availability_zones
  admin_role_arns    = var.admin_role_arns
  subordinate_ca_arn = module.subordinate_ca.ca_arn
  public_api_cidrs   = var.public_api_cidrs
  node_scaling       = { min = 2, desired = 2, max = 4 }
}

module "global" {
  source                    = "../../modules/global"
  environment               = "test"
  admin_role_arns           = var.admin_role_arns
  vpc_id                    = module.cell.vpc_id
  vpc_cidr                  = module.cell.cidr
  private_subnet_ids        = module.cell.private_subnet_ids
  public_subnet_ids         = module.cell.public_subnet_ids
  cluster_security_group_id = module.cell.cluster_security_group_id
}

output "cell" {
  description = "The cell test-1: cluster, database, bucket, keys and workload roles (the erp-cell chart's values, T40c)."
  value = {
    cluster_name               = module.cell.cluster_name
    cluster_endpoint           = module.cell.cluster_endpoint
    database_endpoint          = module.cell.database_endpoint
    database_master_secret_arn = module.cell.database_master_secret_arn
    object_bucket              = module.cell.object_bucket
    field_kms_key_arn          = module.cell.field_kms_key_arn
    cluster_kms_key_arn        = module.cell.cluster_kms_key_arn
    workload_role_arns         = module.cell.workload_role_arns
  }
}

output "global" {
  description = "The directory and the router's load balancer (the erp-global chart's values, T40c)."
  value = {
    directory_endpoint          = module.global.directory_endpoint
    directory_master_secret_arn = module.global.directory_master_secret_arn
    router_dns_name             = module.global.router_dns_name
    router_target_group_arn     = module.global.router_target_group_arn
  }
}

output "subordinate_ca_arn" {
  description = "The environment's subordinate CA (cert-manager's AWSPCAClusterIssuer, T40c)."
  value       = module.subordinate_ca.ca_arn
}

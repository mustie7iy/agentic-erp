# An environment (ADR-0035): its subordinate CA, its home cell and the
# global tier in that cell's VPC, wired together. Every environment root
# (envs/dev, test, staging, production) is this module with its account,
# its cell's name and range, and its shape: dev and test run the smaller
# single-AZ shape (zone_redundant = false), staging and production the
# full one.
#
# Two providers: the default one is the environment's account, `tooling`
# the tooling account, where the root CA signs the subordinate.

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
}

variable "cell" {
  description = "The home cell's id (ERP_CELL's rule), e.g. dev1."
  type        = string
}

variable "cidr" {
  description = "The home cell's VPC range."
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
  description = "The tooling account, home of the image registry and the root CA."
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

variable "zone_redundant" {
  description = "The production shape (staging, production): a Multi-AZ database cluster and directory, a NAT gateway and endpoints per zone. false is dev's and test's smaller single-AZ shape (product owner, T40b)."
  type        = bool
  default     = true
}

variable "backup_retention_days" {
  description = "Days of the cell's point-in-time recovery: 35 in production, 14 elsewhere (ADR-0040)."
  type        = number
  default     = 14
}

variable "dr_region" {
  description = "The cell's disaster-recovery region (ADR-0040): staging and production; null in dev and test."
  type        = string
  default     = null
}

variable "pilot_light" {
  description = "Keep the home cell's pilot light in dr_region (production, ADR-0040)."
  type        = bool
  default     = false
}

variable "dr_cidr" {
  description = "The pilot light's VPC /16 in dr_region."
  type        = string
  default     = null
}

variable "dr_availability_zones" {
  description = "Three availability zones of dr_region, for the pilot light."
  type        = list(string)
  default     = []
}

variable "replica_lag_alarm_actions" {
  description = "SNS topics of dr_region the pilot light's replica-lag alarm notifies."
  type        = list(string)
  default     = []
}

variable "node_scaling" {
  description = "The home cell's node count bounds."
  type = object({
    min     = number
    desired = number
    max     = number
  })
  default = { min = 3, desired = 3, max = 12 }
}

module "subordinate_ca" {
  source = "../subordinate-ca"
  providers = {
    aws         = aws
    aws.tooling = aws.tooling
  }
  environment      = var.environment
  organization     = var.ca_organization
  country          = var.ca_country
  root_ca_arn      = var.root_ca_arn
  root_certificate = var.root_certificate
}

# The account's Auto Scaling service-linked role, which launches the
# nodes' encrypted volumes. AWS creates it with the first Auto Scaling
# group, but the cell's key names it before that, and KMS refuses a policy
# naming a role that does not exist. In an account that already has it,
# import it first (ADR-0035, bootstrap step 5).
resource "aws_iam_service_linked_role" "autoscaling" {
  aws_service_name = "autoscaling.amazonaws.com"
}

module "cell" {
  source                    = "../cell"
  name                      = var.cell
  autoscaling_role_arn      = aws_iam_service_linked_role.autoscaling.arn
  image_registry_account_id = var.tooling_account_id
  cidr                      = var.cidr
  availability_zones        = var.availability_zones
  admin_role_arns           = var.admin_role_arns
  subordinate_ca_arn        = module.subordinate_ca.ca_arn
  public_api_cidrs          = var.public_api_cidrs
  node_scaling              = var.node_scaling
  zone_redundant            = var.zone_redundant
  backup_retention_days     = var.backup_retention_days
  dr_region                 = var.dr_region
  pilot_light               = var.pilot_light
  dr_cidr                   = var.dr_cidr
  dr_availability_zones     = var.dr_availability_zones
  replica_lag_alarm_actions = var.replica_lag_alarm_actions
  # The home cell: the router runs on its cluster (the erp-global chart).
  global_secrets = module.global.external_secrets
}

module "global" {
  source                    = "../global"
  environment               = var.environment
  admin_role_arns           = var.admin_role_arns
  vpc_id                    = module.cell.vpc_id
  vpc_cidr                  = module.cell.cidr
  private_subnet_ids        = module.cell.private_subnet_ids
  public_subnet_ids         = module.cell.public_subnet_ids
  cluster_security_group_id = module.cell.cluster_security_group_id
  # The directory follows the cell's shape: in the single-AZ shape it runs
  # in the first zone, where the cell's database is.
  directory_multi_az = var.zone_redundant
  directory_zone     = var.zone_redundant ? null : var.availability_zones[0]
}

output "cell" {
  description = "The home cell: cluster, database, bucket, keys and workload roles (the erp-cell chart's values)."
  value = {
    cluster_name               = module.cell.cluster_name
    vpc_id                     = module.cell.vpc_id
    cidr                       = module.cell.cidr
    cluster_endpoint           = module.cell.cluster_endpoint
    database_endpoint          = module.cell.database_endpoint
    database_master_secret_arn = module.cell.database_master_secret_arn
    object_bucket              = module.cell.object_bucket
    field_kms_key_arn          = module.cell.field_kms_key_arn
    cluster_kms_key_arn        = module.cell.cluster_kms_key_arn
    workload_role_arns         = module.cell.workload_role_arns
    aws_endpoints              = module.cell.aws_endpoints
  }
}

output "global" {
  description = "The directory and the router's load balancer (the erp-global chart's values)."
  value = {
    directory_endpoint          = module.global.directory_endpoint
    directory_master_secret_arn = module.global.directory_master_secret_arn
    router_dns_name             = module.global.router_dns_name
    router_target_group_arn     = module.global.router_target_group_arn
    router_proxy_sources        = module.global.router_proxy_sources
  }
}

output "subordinate_ca_certificate" {
  description = "The environment's trust anchor (the charts' trust.caBundle, ERP_TLS_CA)."
  value       = module.subordinate_ca.ca_certificate
}

output "subordinate_ca_arn" {
  description = "The environment's subordinate CA (cert-manager's AWSPCAClusterIssuer)."
  value       = module.subordinate_ca.ca_arn
}

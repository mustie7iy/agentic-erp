# The dev environment (ADR-0035): one cell, dev-1, its global tier and the
# environment's subordinate CA. Regions, administrator roles and the CA's
# subject are settled at first apply, in a terraform.tfvars kept out of the
# repository.

variable "account_id" {
  description = "The dev account's id; any other account is refused."
  type        = string
}

variable "region" {
  description = "The region of the cell dev-1."
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
  environment      = "dev"
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
  source               = "../../modules/cell"
  name                 = "dev-1"
  autoscaling_role_arn = aws_iam_service_linked_role.autoscaling.arn
  cidr                 = "10.16.0.0/16"
  availability_zones   = var.availability_zones
  admin_role_arns      = var.admin_role_arns
  subordinate_ca_arn   = module.subordinate_ca.ca_arn
  public_api_cidrs     = var.public_api_cidrs
  node_scaling         = { min = 2, desired = 2, max = 4 }
}

module "global" {
  source                    = "../../modules/global"
  environment               = "dev"
  admin_role_arns           = var.admin_role_arns
  vpc_id                    = module.cell.vpc_id
  vpc_cidr                  = module.cell.cidr
  private_subnet_ids        = module.cell.private_subnet_ids
  public_subnet_ids         = module.cell.public_subnet_ids
  cluster_security_group_id = module.cell.cluster_security_group_id
}

output "cell" {
  description = "The cell dev-1: cluster, database, bucket, keys and workload roles (the erp-cell chart's values, T40c)."
  value = {
    cluster_name               = module.cell.cluster_name
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

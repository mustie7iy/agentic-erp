# A cell (05 §5.4, ADR-0035): a complete copy of the stack serving a set of
# tenants in one region. This module is its infrastructure:
#   - the network, with S3, KMS and STS never proxied (network module);
#   - an EKS cluster whose secrets, volumes and logs are encrypted with the
#     cell's keys (eks.tf);
#   - RDS for PostgreSQL 17 as a Multi-AZ cluster requiring TLS 1.3, the
#     database of the platform and of the cell's Temporal (rds.tf);
#   - the object store bucket and the secrets the workloads read (data.tf);
#   - the roles the workloads assume through their service accounts (IRSA,
#     ADR-0024), each limited to what it calls (irsa.tf).
# NATS JetStream and Temporal run on the cluster (Helm, T40c): their
# volumes come from the EBS CSI driver encrypted with the cluster key, and
# Temporal's databases live on the cell's DB cluster.

variable "name" {
  description = "The cell's name, e.g. dev-1."
  type        = string
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,20}$", var.name))
    error_message = "lower-case letters, digits and hyphens, at most 21 characters"
  }
}

variable "cidr" {
  description = "The cell VPC's /16."
  type        = string
}

variable "availability_zones" {
  description = "Three availability zones of the region."
  type        = list(string)
}

variable "admin_role_arns" {
  description = "Roles that administer the cell's keys and the cluster (platform operators, Argo CD)."
  type        = list(string)
  validation {
    condition     = length(var.admin_role_arns) > 0 && alltrue([for arn in var.admin_role_arns : can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/", arn))])
    error_message = "at least one administrator, each an IAM role"
  }
}

variable "subordinate_ca_arn" {
  description = "The environment's subordinate CA, from which cert-manager issues the cell's certificates (T38)."
  type        = string
}

variable "autoscaling_role_arn" {
  description = "The account's Auto Scaling service-linked role, which launches the encrypted node volumes; it must exist before the cluster key names it (the environment root creates it)."
  type        = string
  validation {
    condition     = can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling$", var.autoscaling_role_arn))
    error_message = "the AWSServiceRoleForAutoScaling role's ARN"
  }
}

variable "kubernetes_version" {
  description = "EKS's Kubernetes version."
  type        = string
  default     = "1.34"
}

variable "public_api_cidrs" {
  description = "CIDRs allowed to reach the Kubernetes API publicly; empty keeps it private (operators come through a VPN or the tooling account)."
  type        = list(string)
  default     = []
}

variable "node_instance_types" {
  description = "Instance types of the cell's nodes."
  type        = list(string)
  default     = ["m7g.xlarge"]
}

variable "node_scaling" {
  description = "Node count bounds."
  type = object({
    min     = number
    desired = number
    max     = number
  })
  default = { min = 3, desired = 3, max = 12 }
}

variable "zone_redundant" {
  description = "The production shape (staging, production): a Multi-AZ DB cluster, a NAT gateway and interface endpoints per zone. False is dev and test: a single-AZ PostgreSQL instance and one zone's egress."
  type        = bool
  default     = true
}

variable "database_instance_class" {
  description = "Instance class of the database in either shape; null picks db.m6gd.large for the Multi-AZ DB cluster, db.t4g.medium for the single instance."
  type        = string
  default     = null
}

variable "database_storage_gb" {
  description = "Allocated gp3 storage; null picks 400 GiB for the Multi-AZ DB cluster (gp3 takes provisioned IOPS from 400 GiB, which the cluster requires), 20 GiB for the single instance."
  type        = number
  default     = null
  validation {
    condition     = var.database_storage_gb == null || (var.zone_redundant ? coalesce(var.database_storage_gb, 0) >= 400 : coalesce(var.database_storage_gb, 0) >= 20)
    error_message = "the Multi-AZ DB cluster needs at least 400 GiB (below it gp3 refuses provisioned IOPS), the single instance 20 GiB (gp3's minimum)"
  }
}

variable "database_iops" {
  description = "Provisioned IOPS of the Multi-AZ DB cluster's gp3 storage (12000 is gp3's baseline from 400 GiB); the single instance takes gp3's baseline and ignores it."
  type        = number
  default     = 12000
}

variable "log_retention_days" {
  description = "How long the cluster's and the database's logs are kept."
  type        = number
  default     = 90
}

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}
data "aws_region" "current" {}

locals {
  prefix       = "erp-${var.name}"
  account_root = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root"
}

module "network" {
  source             = "../network"
  name               = local.prefix
  cidr               = var.cidr
  availability_zones = var.availability_zones
  zone_redundant     = var.zone_redundant
}

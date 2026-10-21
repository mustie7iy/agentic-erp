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
  description = "The cell's name, e.g. dev1."
  type        = string
  validation {
    # The platform's own rule for ERP_CELL, which the cell's name is
    # (config: 1-20 lower-case letters or digits), and its SPIFFE path.
    condition     = can(regex("^[a-z][a-z0-9]{1,19}$", var.name))
    error_message = "a letter, then lower-case letters or digits, at most 20 characters: ERP_CELL's rule"
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

variable "image_registry_account_id" {
  description = "The account whose registry holds the release images (tooling), which Kyverno reads signatures from."
  type        = string
  validation {
    condition     = can(regex("^[0-9]{12}$", var.image_registry_account_id))
    error_message = "a 12-digit account id"
  }
}

variable "vpc_cni_addon_version" {
  description = "The EKS add-on version of the VPC CNI; null takes EKS's default for the cluster's Kubernetes version."
  type        = string
  default     = null
}

variable "ebs_csi_addon_version" {
  description = "The EKS add-on version of the EBS CSI driver; null takes EKS's default for the cluster's Kubernetes version."
  type        = string
  default     = null
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

variable "backup_retention_days" {
  description = "Days of point-in-time recovery: 35 in production, 14 elsewhere (ADR-0040)."
  type        = number
  default     = 14
  validation {
    # RDS keeps at most 35 days; fewer than 14 is below every environment's.
    condition     = var.backup_retention_days >= 14 && var.backup_retention_days <= 35
    error_message = "between 14 and 35 days"
  }
}

variable "dr_region" {
  description = "The disaster-recovery region the database's backups are copied to and the bucket replicates to (ADR-0040): staging and production. null (dev and test): none."
  type        = string
  default     = null
  validation {
    # The copies are of the Multi-AZ DB cluster (AWS Backup), the production shape's database.
    condition     = var.dr_region == null || var.zone_redundant
    error_message = "a disaster-recovery region needs the production shape (zone_redundant)"
  }
}

variable "log_retention_days" {
  description = "How long the cluster's, the database's and the containers' logs are kept."
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

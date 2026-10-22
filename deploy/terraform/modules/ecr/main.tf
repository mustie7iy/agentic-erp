# The image registry (ADR-0032, ADR-0035): one ECR repository per binary in
# the tooling account's primary region, replicated to every region a cell
# runs in, readable by the environment accounts.
#
# - Release tags are immutable: vX.Y.Z names one digest forever. cosign's
#   signature and attestation tags (sha256-<digest>.sig, .att) are excluded,
#   because signing a release again adds to them.
# - Encrypted with a key of the account's in the primary region; replicas
#   are created by ECR from a creation template, encrypted with KMS too.
# - The lifecycle policy only ever expires release tags beyond the number
#   kept. It never expires untagged manifests: a signature stored as an OCI
#   referrer is untagged, and deleting it would make its image unverifiable.

variable "repositories" {
  description = "Repository names: one per binary, and one per upstream image the cells run (deploy/images/upstream.txt), copied and signed by the release (scripts/mirror-images.sh)."
  type        = list(string)
  default     = ["erp-server", "erp-router", "erp-web", "erp", "erp-analytics", "erp-pgbouncer", "erp-nats"]
  validation {
    condition     = alltrue([for name in var.repositories : startswith(name, "erp")])
    error_message = "every repository's name starts with erp: the cells' roles read erp* (modules/cell irsa.tf)"
  }
}

variable "replication_regions" {
  description = "Regions the images are replicated to (the cells' regions other than this one)."
  type        = list(string)
  default     = []
}

variable "pull_account_ids" {
  description = "Accounts that may pull the images (the environment accounts)."
  type        = list(string)
  default     = []
}

variable "releases_kept" {
  description = "How many release tags each repository keeps."
  type        = number
  default     = 100
}

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

resource "aws_kms_key" "ecr" {
  description             = "Release images (ADR-0035)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AccountAdministers"
      Effect    = "Allow"
      Principal = { AWS = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root" }
      Action    = "kms:*"
      Resource  = "*"
    }]
  })
}

resource "aws_kms_alias" "ecr" {
  name          = "alias/erp-images"
  target_key_id = aws_kms_key.ecr.key_id
}

resource "aws_ecr_repository" "this" {
  for_each             = toset(var.repositories)
  name                 = each.key
  image_tag_mutability = "IMMUTABLE_WITH_EXCLUSION"
  image_tag_mutability_exclusion_filter {
    filter      = "sha256-*"
    filter_type = "WILDCARD"
  }
  image_scanning_configuration {
    scan_on_push = true
  }
  encryption_configuration {
    encryption_type = "KMS"
    kms_key         = aws_kms_key.ecr.arn
  }
}

resource "aws_ecr_lifecycle_policy" "this" {
  for_each   = aws_ecr_repository.this
  repository = each.value.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the latest ${var.releases_kept} release tags; never touch untagged manifests (signatures)"
      selection = {
        tagStatus     = "tagged"
        tagPrefixList = ["v"]
        countType     = "imageCountMoreThan"
        countNumber   = var.releases_kept
      }
      action = { type = "expire" }
    }]
  })
}

locals {
  # What the environments may do on the images: pull them (the nodes), and
  # list and describe them, which reading a signature or attestation needs
  # (Kyverno's admission controller follows the OCI referrers, or cosign's
  # sha256-<digest> tags, T40c). Nothing that writes or deletes.
  pull_actions = [
    "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability",
    "ecr:DescribeImages", "ecr:ListImages",
  ]
}

resource "aws_ecr_repository_policy" "pull" {
  for_each   = length(var.pull_account_ids) == 0 ? {} : aws_ecr_repository.this
  repository = each.value.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "EnvironmentsPull"
      Effect    = "Allow"
      Principal = { AWS = [for id in var.pull_account_ids : "arn:${data.aws_partition.current.partition}:iam::${id}:root"] }
      Action    = local.pull_actions
    }]
  })
}

# A creation template applies in the region where ECR creates the replica,
# so there is one in every destination region (the provider's per-resource
# region); without it a replica would get AES256, mutable tags and no pull
# policy for the environment accounts.
resource "aws_ecr_repository_creation_template" "replicas" {
  for_each             = toset(var.replication_regions)
  region               = each.value
  prefix               = "ROOT"
  description          = "Replicas of the release images (ADR-0035)"
  applied_for          = ["REPLICATION"]
  image_tag_mutability = "IMMUTABLE_WITH_EXCLUSION"
  image_tag_mutability_exclusion_filter {
    filter      = "sha256-*"
    filter_type = "WILDCARD"
  }
  encryption_configuration {
    encryption_type = "KMS"
  }
  repository_policy = length(var.pull_account_ids) == 0 ? null : jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "EnvironmentsPull"
      Effect    = "Allow"
      Principal = { AWS = [for id in var.pull_account_ids : "arn:${data.aws_partition.current.partition}:iam::${id}:root"] }
      Action    = local.pull_actions
    }]
  })
}

resource "aws_ecr_replication_configuration" "this" {
  count = length(var.replication_regions) == 0 ? 0 : 1
  replication_configuration {
    rule {
      dynamic "destination" {
        for_each = var.replication_regions
        content {
          region      = destination.value
          registry_id = data.aws_caller_identity.current.account_id
        }
      }
      # Each repository by name, as a prefix (ECR's only filter type): a
      # name that is a prefix of another (erp of erp-server) also covers
      # it, which replicates the same set.
      dynamic "repository_filter" {
        for_each = var.repositories
        content {
          filter      = repository_filter.value
          filter_type = "PREFIX_MATCH"
        }
      }
    }
  }
  depends_on = [aws_ecr_repository_creation_template.replicas]
}

output "repository_arns" {
  description = "The repositories, by name."
  value       = { for name, repository in aws_ecr_repository.this : name => repository.arn }
}

output "repository_urls" {
  description = "Where the release workflow pushes, by name."
  value       = { for name, repository in aws_ecr_repository.this : name => repository.repository_url }
}

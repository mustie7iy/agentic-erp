# The roles GitHub Actions assumes with its OIDC token (ADR-0032, ADR-0035):
#   - release: the release workflow in the protected `release` environment;
#     signs with the KMS key and pushes the images to ECR;
#   - delivery: promotion checks in the `delivery` environment and, later,
#     Argo CD (extra principals); reads images and the public key only.
# Each trust policy names the repository and the environment exactly, so a
# workflow elsewhere, or outside the environment's approval, gets nothing.
# The roles exist before the key (its policy names them); their permission
# policies name the key and are attached afterwards.

variable "repository" {
  description = "The GitHub repository, owner/name."
  type        = string
}

variable "create_oidc_provider" {
  description = "Create GitHub's OIDC provider in this account (false when it exists already)."
  type        = bool
  default     = true
}

variable "oidc_provider_arn" {
  description = "GitHub's OIDC provider, when it exists already."
  type        = string
  default     = ""
}

variable "signing_key_arn" {
  description = "The release signing key (signing-key module)."
  type        = string
}

variable "repository_arns" {
  description = "The ECR repositories the release pushes to and delivery reads."
  type        = list(string)
}

variable "delivery_principal_arns" {
  description = "Further roles that may assume the delivery role (Argo CD's role, T40d)."
  type        = list(string)
  default     = []
  validation {
    condition     = alltrue([for arn in var.delivery_principal_arns : can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/", arn))])
    error_message = "delivery is shared with IAM roles (arn:aws:iam::<account>:role/...), never a whole account"
  }
}

locals {
  issuer       = "token.actions.githubusercontent.com"
  provider_arn = var.create_oidc_provider ? aws_iam_openid_connect_provider.github[0].arn : var.oidc_provider_arn
  ecr_read     = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability", "ecr:DescribeImages", "ecr:ListImages"]
  ecr_write    = ["ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload", "ecr:PutImage"]
}

resource "aws_iam_openid_connect_provider" "github" {
  count          = var.create_oidc_provider ? 1 : 0
  url            = "https://${local.issuer}"
  client_id_list = ["sts.amazonaws.com"]
}

# github_trust is the trust policy for a GitHub Actions environment.
locals {
  github_trust = { for environment in ["release", "delivery"] : environment => {
    Effect    = "Allow"
    Principal = { Federated = local.provider_arn }
    Action    = "sts:AssumeRoleWithWebIdentity"
    Condition = {
      StringEquals = {
        "${local.issuer}:aud" = "sts.amazonaws.com"
        "${local.issuer}:sub" = "repo:${var.repository}:environment:${environment}"
      }
    }
  } }
}

resource "aws_iam_role" "release" {
  name                 = "erp-release"
  description          = "The release workflow: sign with the KMS key, push to ECR (ADR-0032)"
  max_session_duration = 3600
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [local.github_trust["release"]]
  })
}

resource "aws_iam_role_policy" "release" {
  name = "sign-and-push"
  role = aws_iam_role.release.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Sid = "Sign", Effect = "Allow", Action = ["kms:Sign", "kms:GetPublicKey", "kms:DescribeKey"], Resource = var.signing_key_arn },
      { Sid = "Login", Effect = "Allow", Action = "ecr:GetAuthorizationToken", Resource = "*" },
      { Sid = "Push", Effect = "Allow", Action = concat(local.ecr_read, local.ecr_write), Resource = var.repository_arns },
    ]
  })
}

resource "aws_iam_role" "delivery" {
  name                 = "erp-delivery"
  description          = "Delivery: read images and the signing key's public half (ADR-0035)"
  max_session_duration = 3600
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([local.github_trust["delivery"]], length(var.delivery_principal_arns) == 0 ? [] : [{
      Effect    = "Allow"
      Principal = { AWS = var.delivery_principal_arns }
      Action    = "sts:AssumeRole"
    }])
  })
}

resource "aws_iam_role_policy" "delivery" {
  name = "read"
  role = aws_iam_role.delivery.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Sid = "PublicKey", Effect = "Allow", Action = ["kms:GetPublicKey", "kms:DescribeKey"], Resource = var.signing_key_arn },
      { Sid = "Login", Effect = "Allow", Action = "ecr:GetAuthorizationToken", Resource = "*" },
      { Sid = "Pull", Effect = "Allow", Action = local.ecr_read, Resource = var.repository_arns },
    ]
  })
}

output "release_role_arn" {
  description = "ERP_RELEASE_ROLE_ARN of the release environment."
  value       = aws_iam_role.release.arn
}

output "delivery_role_arn" {
  description = "The delivery role."
  value       = aws_iam_role.delivery.arn
}

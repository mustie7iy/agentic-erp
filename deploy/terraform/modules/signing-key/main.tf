# The key that signs release images (ADR-0032, ADR-0035): an asymmetric
# ECC_NIST_P256 key for SIGN_VERIFY in the tooling account. Its policy is
# the whole access model: administrators manage it but cannot sign, the
# release role signs and reads the public key, verifiers (delivery, the
# clusters' admission policy) only read the public key. Asymmetric keys do
# not rotate in place; rotation is a new key and re-signing (ADR-0032).

variable "alias" {
  description = "The key's alias, without the alias/ prefix."
  type        = string
  default     = "erp-release-signing"
}

variable "admin_role_arns" {
  description = "Roles that administer the key (not sign with it). The role that applies this module must be one of them: KMS refuses a key policy that would lock its creator out."
  type        = list(string)
  validation {
    condition     = length(var.admin_role_arns) > 0
    error_message = "a signing key needs an administrator, or it can never be changed or retired"
  }
  validation {
    condition     = alltrue([for arn in var.admin_role_arns : can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/", arn))])
    error_message = "administrators are IAM roles (arn:aws:iam::<account>:role/...), never an account root"
  }
}

variable "signer_role_arns" {
  description = "Roles that may sign with the key: the release role."
  type        = list(string)
}

variable "verifier_role_arns" {
  description = "Roles that may read the public key: delivery and the clusters."
  type        = list(string)
  default     = []
}

# No account-root statement: IAM policies grant nothing on this key, which
# is what keeps administrators from signing. If every administrator role
# were deleted, only AWS Support could restore access (ADR-0035 runbook).
resource "aws_kms_key" "signing" {
  description              = "Signs release images (ADR-0032)"
  customer_master_key_spec = "ECC_NIST_P256"
  key_usage                = "SIGN_VERIFY"
  deletion_window_in_days  = 30
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid       = "Administer"
        Effect    = "Allow"
        Principal = { AWS = var.admin_role_arns }
        Action = [
          "kms:Describe*", "kms:Get*", "kms:List*", "kms:Put*", "kms:Update*", "kms:Enable*", "kms:Disable*",
          "kms:CreateAlias", "kms:DeleteAlias",
          "kms:TagResource", "kms:UntagResource", "kms:ScheduleKeyDeletion", "kms:CancelKeyDeletion",
        ]
        Resource = "*"
      },
      {
        Sid       = "Sign"
        Effect    = "Allow"
        Principal = { AWS = var.signer_role_arns }
        Action    = ["kms:Sign", "kms:GetPublicKey", "kms:DescribeKey"]
        Resource  = "*"
      },
      ], length(var.verifier_role_arns) == 0 ? [] : [{
        Sid       = "Verify"
        Effect    = "Allow"
        Principal = { AWS = var.verifier_role_arns }
        Action    = ["kms:GetPublicKey", "kms:DescribeKey"]
        Resource  = "*"
    }])
  })
  # Deleting it would leave every deployed image unverifiable.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "signing" {
  name          = "alias/${var.alias}"
  target_key_id = aws_kms_key.signing.key_id
}

output "key_arn" {
  description = "The signing key."
  value       = aws_kms_key.signing.arn
}

output "policy" {
  description = "The key policy, the key's whole access model (for review and tests)."
  value       = aws_kms_key.signing.policy
}

output "cosign_key" {
  description = "The value of ERP_COSIGN_KEY and ERP_COSIGN_PUBLIC_KEY (ADR-0032)."
  value       = "awskms:///${aws_kms_key.signing.arn}"
}

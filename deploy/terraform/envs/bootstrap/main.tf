# The state bucket every root keeps its state in (ADR-0035), in the tooling
# account. Its own state cannot start in the bucket it creates: the first
# apply runs with a backend_override.tf declaring `backend "local" {}`; the
# override is then deleted and `terraform init -migrate-state` moves the
# state into the bucket (the runbook in ADR-0035).

variable "account_id" {
  description = "The tooling account's id; any other account is refused."
  type        = string
}

variable "region" {
  description = "The tooling account's primary region."
  type        = string
}

variable "bucket" {
  description = "The state bucket's name (globally unique)."
  type        = string
}

variable "admin_role_arns" {
  description = "Roles that administer the state key besides the account root."
  type        = list(string)
  default     = []
}

module "state" {
  source          = "../../modules/state"
  bucket          = var.bucket
  admin_role_arns = var.admin_role_arns
}

output "bucket" {
  description = "The bucket for every root's -backend-config."
  value       = module.state.bucket
}

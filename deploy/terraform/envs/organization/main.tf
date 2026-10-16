# The accounts (ADR-0035): tooling and one per environment, applied from
# the organization's management account. Their ids feed envs/tooling
# (who may pull the images) and the environment roots (T40b).

variable "account_id" {
  description = "The management account's id; any other account is refused."
  type        = string
}

variable "region" {
  description = "The region the provider talks to (Organizations is global)."
  type        = string
}

variable "parent_id" {
  description = "The organization root (r-...) the erp OU is created under."
  type        = string
}

variable "accounts" {
  description = "Root e-mail address of each account: tooling, dev, test, staging, production."
  type = map(object({
    email = string
  }))
}

module "accounts" {
  source    = "../../modules/accounts"
  parent_id = var.parent_id
  accounts  = var.accounts
}

output "account_ids" {
  description = "Account ids by environment name."
  value       = module.accounts.account_ids
}

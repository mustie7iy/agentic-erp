# One AWS account per environment plus a tooling account (ADR-0035), in an
# organizational unit of their own under the organization's root. Applied
# from the management account. An account cannot be recreated under the
# same e-mail address, so none is ever destroyed by Terraform.

variable "parent_id" {
  description = "The organization root (r-...) or OU the workloads OU is created under."
  type        = string
}

variable "accounts" {
  description = "Accounts by name (tooling, dev, test, staging, production) with their root e-mail address."
  type = map(object({
    email = string
  }))
  validation {
    condition     = alltrue([for name in ["tooling", "dev", "test", "staging", "production"] : contains(keys(var.accounts), name)])
    error_message = "the accounts are tooling, dev, test, staging and production (ADR-0035)"
  }
}

resource "aws_organizations_organizational_unit" "workloads" {
  name      = "erp"
  parent_id = var.parent_id
}

resource "aws_organizations_account" "this" {
  for_each          = var.accounts
  name              = "erp-${each.key}"
  email             = each.value.email
  parent_id         = aws_organizations_organizational_unit.workloads.id
  role_name         = "OrganizationAccountAccessRole"
  close_on_deletion = false
  tags = {
    environment = each.key
  }
  lifecycle {
    prevent_destroy = true
    ignore_changes  = [role_name]
  }
}

output "account_ids" {
  description = "Account ids by environment name."
  value       = { for name, account in aws_organizations_account.this : name => account.id }
}

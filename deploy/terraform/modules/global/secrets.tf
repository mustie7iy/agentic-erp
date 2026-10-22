# The global tier's secrets (the erp-global chart), under erp/global/, read
# through the External Secrets Operator of the home cell's cluster as
# mounted files (ADR-0017): the global plane's page-token key, and the
# router's and the web workspace's public certificate chains and keys
# (ADR-0052), which operators load from a public CA (ADR-0035: no ACME). Created empty, encrypted with the global
# tier's key, which also encrypts the directory and its RDS-managed master
# credentials; no value is in Terraform's state.

variable "secrets" {
  description = "Names of the global tier's secrets, under erp/global/ (values are set outside Terraform)."
  type        = list(string)
  default     = ["page-token-key", "router-public-certificate", "router-public-key", "web-public-certificate", "web-public-key"]
}

resource "aws_secretsmanager_secret" "global" {
  for_each                = toset(var.secrets)
  name                    = "erp/global/${each.key}"
  kms_key_id              = aws_kms_key.directory.arn
  recovery_window_in_days = 30
}

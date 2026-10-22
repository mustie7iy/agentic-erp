# The global tier's secrets (the erp-global chart), under erp/global/, read
# through the External Secrets Operator of the home cell's cluster as
# mounted files (ADR-0017): the global plane's page-token key, the
# router's and the web workspace's public certificate chains and keys
# (ADR-0052), which operators load from a public CA (ADR-0035: no ACME),
# the web workspace's session sealing key (web-session-key, at least 32
# random bytes; web-session-key-previous during a rotation, ADR-0028) and
# its clients' credentials at the tenants' identity
# providers (web-clients: a JSON object of file name to PEM key or
# secret, which web_client credential_ref names as
# file:/etc/erp/web-clients/<name>, T73b). Created empty, encrypted with
# the global tier's key, which also encrypts the directory and its
# RDS-managed master credentials; no value is in Terraform's state.

variable "secrets" {
  description = "Names of the global tier's secrets, under erp/global/ (values are set outside Terraform)."
  type        = list(string)
  default     = ["page-token-key", "router-public-certificate", "router-public-key", "web-public-certificate", "web-public-key", "web-session-key", "web-session-key-previous", "web-clients"]
}

resource "aws_secretsmanager_secret" "global" {
  for_each                = toset(var.secrets)
  name                    = "erp/global/${each.key}"
  kms_key_id              = aws_kms_key.directory.arn
  recovery_window_in_days = 30
}

# The cell's container logs (ADR-0043, T54g): every container's stdout,
# shipped by the fluent-bit add-on (deploy/helm/addons/values/fluent-bit.yaml)
# through the CloudWatch Logs VPC endpoint as its own role (irsa.tf
# logging), kept log_retention_days (90) and encrypted with the cluster
# key, whose policy admits CloudWatch Logs for this account's groups
# (kms.tf). The server's request lines carry tenant and principal as
# fields; this is where operators find which tenant is slow.

resource "aws_cloudwatch_log_group" "containers" {
  name              = "/erp/${var.name}/containers"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.cluster.arn
}

output "directory_endpoint" {
  description = "The directory database (host:port), ERP_DIRECTORY_URL's host."
  value       = aws_db_instance.directory.endpoint
}

output "directory_master_secret_arn" {
  description = "The RDS-managed master credentials of the directory in Secrets Manager: the erp-global chart's setup hook's alone (T72h)."
  value       = one(aws_db_instance.directory.master_user_secret[*].secret_arn)
}

output "directory_users" {
  description = "The rds-db:connect resource of each client's directory login (router, cell, web): the cell module's directory_users, for its IRSA roles."
  value       = local.directory_users
}

output "directory_region" {
  description = "The directory's region, which its IAM tokens are signed for (ERP_DIRECTORY_REGION: the erp-global chart's directory.region, erp-cell's directory.region)."
  value       = data.aws_region.current.region
}

output "router_dns_name" {
  description = "The load balancer's DNS name, the target of the API's public record."
  value       = aws_lb.router.dns_name
}

output "router_target_group_arn" {
  description = "Where the erp-global chart's TargetGroupBinding registers the router's pods (T40c)."
  value       = aws_lb_target_group.router.arn
}

output "router_proxy_sources" {
  description = "The load balancer's subnets, whose connections carry a PROXY v2 header: the erp-global chart's router.proxyFrom (ERP_ROUTER_PROXY_FROM)."
  value       = local.balancer_cidrs
  precondition {
    condition     = length(local.pods_in_balancer_subnets) == 0
    error_message = "The cluster's private subnets overlap the load balancers' (${join(", ", local.pods_in_balancer_subnets)}): a pod's connection would be taken for the balancer's, and erp-web's calls to the router carry no PROXY header (T73b)."
  }
}

output "web_dns_name" {
  description = "The web workspace's load balancer's DNS name, the target of its hostname's public record (app.<domain>, ADR-0052)."
  value       = aws_lb.web.dns_name
}

output "web_target_group_arn" {
  description = "Where the erp-global chart's TargetGroupBinding registers erp-web's pods (web.targetGroupArn)."
  value       = aws_lb_target_group.web.arn
}

output "web_proxy_sources" {
  description = "The web load balancer's subnets, whose connections carry a PROXY v2 header: the erp-global chart's web.proxyFrom (ERP_WEB_PROXY_FROM)."
  value       = local.balancer_cidrs
  precondition {
    condition     = length(local.pods_in_balancer_subnets) == 0
    error_message = "The cluster's private subnets overlap the load balancers' (${join(", ", local.pods_in_balancer_subnets)}) (T73b)."
  }
}

output "external_secrets" {
  description = "What the home cell's External Secrets Operator reads for the erp-global chart: the global secrets, the directory's master credentials, and the key encrypting them (the cell module's global_secrets)."
  value = {
    arns        = concat([for s in aws_secretsmanager_secret.global : s.arn], compact([one(aws_db_instance.directory.master_user_secret[*].secret_arn)]))
    kms_key_arn = aws_kms_key.directory.arn
  }
}

output "directory_multi_az" {
  description = "Whether the directory has a standby in another zone (the production shape)."
  value       = aws_db_instance.directory.multi_az
}

output "directory_zone" {
  description = "The directory's zone (its primary's, in the production shape)."
  value       = aws_db_instance.directory.availability_zone
}

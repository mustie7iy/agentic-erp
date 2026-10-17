output "directory_endpoint" {
  description = "The directory database (host:port), ERP_DIRECTORY_URL's host."
  value       = aws_db_instance.directory.endpoint
}

output "directory_master_secret_arn" {
  description = "The RDS-managed master credentials of the directory in Secrets Manager."
  value       = one(aws_db_instance.directory.master_user_secret[*].secret_arn)
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
  value       = sort(distinct([for s in data.aws_subnet.public : s.cidr_block]))
}

output "cluster_name" {
  description = "The cell's EKS cluster."
  value       = aws_eks_cluster.cell.name
}

output "cluster_endpoint" {
  description = "The Kubernetes API (private unless public_api_cidrs is set)."
  value       = aws_eks_cluster.cell.endpoint
}

output "vpc_id" {
  description = "The cell's VPC, where the environment's router load balancer lives too."
  value       = module.network.vpc_id
}

output "public_subnet_ids" {
  description = "The cell's public subnets."
  value       = module.network.public_subnet_ids
}

output "private_subnet_ids" {
  description = "The cell's private subnets."
  value       = module.network.private_subnet_ids
}

output "cidr" {
  description = "The cell's VPC CIDR."
  value       = module.network.cidr
}

output "cluster_security_group_id" {
  description = "The security group EKS gives the cluster's nodes and pods."
  value       = aws_eks_cluster.cell.vpc_config[0].cluster_security_group_id
}

output "database_endpoint" {
  description = "The DB cluster's writer endpoint (PgBouncer's upstream)."
  value       = aws_rds_cluster.cell.endpoint
}

output "database_reader_endpoint" {
  description = "The DB cluster's reader endpoint."
  value       = aws_rds_cluster.cell.reader_endpoint
}

output "database_master_secret_arn" {
  description = "The RDS-managed master credentials in Secrets Manager."
  value       = one(aws_rds_cluster.cell.master_user_secret[*].secret_arn)
}

output "object_bucket" {
  description = "ERP_OBJECT bucket of the cell (ADR-0016)."
  value       = aws_s3_bucket.objects.bucket
}

output "field_kms_key_arn" {
  description = "ERP_FIELD_KMS_KEY of the cell (ADR-0019)."
  value       = aws_kms_key.fields.arn
}

output "cluster_kms_key_arn" {
  description = "The key for the cluster's volumes (the StorageClass of NATS JetStream, T40c)."
  value       = aws_kms_key.cluster.arn
}

output "aws_endpoints" {
  description = "The regional endpoints the workloads must use (ERP_AWS_STS_ENDPOINT, ERP_FIELD_KMS_ENDPOINT, ERP_OBJECT_S3_ENDPOINT, T40c): the VPC endpoints' private DNS answers for regional names only, and the global sts.amazonaws.com would leave through the NAT gateway."
  value = {
    sts = "https://sts.${data.aws_region.current.region}.amazonaws.com"
    kms = "https://kms.${data.aws_region.current.region}.amazonaws.com"
    s3  = "https://s3.${data.aws_region.current.region}.amazonaws.com"
  }
}

output "workload_role_arns" {
  description = "IRSA roles by workload, for the service accounts' eks.amazonaws.com/role-arn annotation (T40c)."
  value       = { for name, role in aws_iam_role.workload : name => role.arn }
}

variable "environment" {
  description = "The environment (dev, test, staging, production)."
  type        = string
  validation {
    condition     = contains(["dev", "test", "staging", "production"], var.environment)
    error_message = "one of dev, test, staging, production"
  }
}

variable "admin_role_arns" {
  description = "Roles that administer the directory's key."
  type        = list(string)
  validation {
    condition     = length(var.admin_role_arns) > 0 && alltrue([for arn in var.admin_role_arns : can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:role/", arn))])
    error_message = "at least one IAM role ARN"
  }
}

variable "vpc_id" {
  description = "The home cell's VPC (the cell module's vpc_id)."
  type        = string
}

variable "vpc_cidr" {
  description = "The home cell's VPC CIDR, from which the router and the cells' servers reach the directory."
  type        = string
}

variable "private_subnet_ids" {
  description = "The home cell's private subnets, for the directory database."
  type        = list(string)
}

variable "public_subnet_ids" {
  description = "The home cell's public subnets, for the load balancer."
  type        = list(string)
}

variable "cluster_security_group_id" {
  description = "The home cell's cluster security group, which the router's pods use."
  type        = string
}

variable "client_cidrs" {
  description = "Who may connect to the load balancer."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "router_port" {
  description = "The router's listening port (ERP_ROUTER_ADDR)."
  type        = number
  default     = 8081
}

variable "directory_instance_class" {
  description = "The directory database's instance class: small, read mostly and cached at the router."
  type        = string
  default     = "db.t4g.medium"
}

variable "log_retention_days" {
  description = "How long the directory's PostgreSQL logs are kept."
  type        = number
  default     = 90
}

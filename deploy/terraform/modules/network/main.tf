# A cell's network (ADR-0035): a VPC across three availability zones with
# public subnets (the load balancers, one NAT gateway per zone) and private
# subnets (nodes, databases, endpoints).
#
# Providers are never proxied (T38b): S3 is reached through a gateway
# endpoint, KMS, STS, ECR, Secrets Manager, CloudWatch Logs and EC2 through
# interface endpoints with private DNS, so the platform's calls to them
# never leave AWS's network. The NAT gateways carry only what has no
# endpoint: the tenants' identity providers, banks, model APIs.

variable "name" {
  description = "The cell's name, e.g. dev-1; prefixes every resource."
  type        = string
}

variable "cidr" {
  description = "The VPC's CIDR block, a /16 the other cells and environments do not use."
  type        = string
  validation {
    condition     = can(cidrhost(var.cidr, 0)) && endswith(var.cidr, "/16")
    error_message = "a /16 CIDR block"
  }
}

variable "availability_zones" {
  description = "Three availability zones of the region."
  type        = list(string)
  validation {
    condition     = length(var.availability_zones) == 3
    error_message = "a cell spans three availability zones"
  }
}

variable "more_interface_endpoints" {
  description = "Services reached through interface endpoints besides KMS and STS (which always are) and S3 (a gateway endpoint)."
  type        = list(string)
  default     = ["ecr.api", "ecr.dkr", "secretsmanager", "logs", "ec2", "elasticloadbalancing"]
  validation {
    condition     = length(setintersection(toset(var.more_interface_endpoints), toset(["kms", "sts", "s3"]))) == 0
    error_message = "S3, KMS and STS have endpoints of their own here"
  }
}

data "aws_region" "current" {}

resource "aws_vpc" "cell" {
  cidr_block           = var.cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = var.name }
}

resource "aws_subnet" "public" {
  for_each                = { for i, az in var.availability_zones : az => i }
  vpc_id                  = aws_vpc.cell.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.cidr, 4, each.value)
  map_public_ip_on_launch = false
  tags                    = { Name = "${var.name}-public-${each.key}", "kubernetes.io/role/elb" = "1" }
}

resource "aws_subnet" "private" {
  for_each          = { for i, az in var.availability_zones : az => i }
  vpc_id            = aws_vpc.cell.id
  availability_zone = each.key
  cidr_block        = cidrsubnet(var.cidr, 2, each.value + 1)
  tags              = { Name = "${var.name}-private-${each.key}", "kubernetes.io/role/internal-elb" = "1" }
}

resource "aws_internet_gateway" "cell" {
  vpc_id = aws_vpc.cell.id
  tags   = { Name = var.name }
}

resource "aws_eip" "nat" {
  for_each = aws_subnet.public
  domain   = "vpc"
  tags     = { Name = "${var.name}-nat-${each.key}" }
}

resource "aws_nat_gateway" "cell" {
  for_each      = aws_subnet.public
  subnet_id     = each.value.id
  allocation_id = aws_eip.nat[each.key].id
  tags          = { Name = "${var.name}-${each.key}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.cell.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.cell.id
  }
  tags = { Name = "${var.name}-public" }
}

resource "aws_route_table_association" "public" {
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  for_each = aws_subnet.private
  vpc_id   = aws_vpc.cell.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.cell[each.key].id
  }
  tags = { Name = "${var.name}-private-${each.key}" }
}

resource "aws_route_table_association" "private" {
  for_each       = aws_subnet.private
  subnet_id      = each.value.id
  route_table_id = aws_route_table.private[each.key].id
}

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.cell.id
  service_name      = "com.amazonaws.${data.aws_region.current.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [for table in aws_route_table.private : table.id]
  tags              = { Name = "${var.name}-s3" }
}

resource "aws_security_group" "endpoints" {
  name        = "${var.name}-endpoints"
  description = "HTTPS from the cell to the interface endpoints"
  vpc_id      = aws_vpc.cell.id
  ingress {
    description = "HTTPS from the VPC"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.cidr]
  }
}

# KMS and STS are written out, not looped over, so deploy/policy/endpoints
# .rego can see them: they and S3 are the services the platform's own
# clients call (ADR-0016, ADR-0019, ADR-0024).
resource "aws_vpc_endpoint" "kms" {
  vpc_id              = aws_vpc.cell.id
  service_name        = "com.amazonaws.${data.aws_region.current.region}.kms"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = [for subnet in aws_subnet.private : subnet.id]
  security_group_ids  = [aws_security_group.endpoints.id]
  tags                = { Name = "${var.name}-kms" }
}

resource "aws_vpc_endpoint" "sts" {
  vpc_id              = aws_vpc.cell.id
  service_name        = "com.amazonaws.${data.aws_region.current.region}.sts"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = [for subnet in aws_subnet.private : subnet.id]
  security_group_ids  = [aws_security_group.endpoints.id]
  tags                = { Name = "${var.name}-sts" }
}

resource "aws_vpc_endpoint" "interface" {
  for_each            = toset(var.more_interface_endpoints)
  vpc_id              = aws_vpc.cell.id
  service_name        = "com.amazonaws.${data.aws_region.current.region}.${each.key}"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = [for subnet in aws_subnet.private : subnet.id]
  security_group_ids  = [aws_security_group.endpoints.id]
  tags                = { Name = "${var.name}-${each.key}" }
}

output "vpc_id" {
  description = "The cell's VPC."
  value       = aws_vpc.cell.id
}

output "private_subnet_ids" {
  description = "Subnets for nodes, databases and internal load balancers."
  value       = [for subnet in aws_subnet.private : subnet.id]
}

output "public_subnet_ids" {
  description = "Subnets for internet-facing load balancers."
  value       = [for subnet in aws_subnet.public : subnet.id]
}

output "cidr" {
  description = "The VPC's CIDR block."
  value       = var.cidr
}

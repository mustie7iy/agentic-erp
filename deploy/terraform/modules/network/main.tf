# A cell's network (ADR-0035): a VPC across three availability zones with
# public subnets (the load balancers, one NAT gateway per zone), private
# subnets (nodes, databases, endpoints) and, for STS's interface endpoint
# alone, a /28 per zone of egress (sts_endpoint_cidrs). Without zone_redundant (dev and
# test) one NAT gateway and the interface endpoints sit in the first zone
# only: a zone's outage takes the cell's egress with it, at a third of the
# cost.
#
# Providers are never proxied (T38b): S3 is reached through a gateway
# endpoint, KMS, STS, ECR, Secrets Manager, CloudWatch Logs, X-Ray and EC2 through
# interface endpoints with private DNS, so the platform's calls to them
# never leave AWS's network. The NAT gateways carry only what has no
# endpoint: the tenants' identity providers, banks, model APIs.

variable "name" {
  description = "The cell's name, e.g. dev1; prefixes every resource."
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
  default     = ["ecr.api", "ecr.dkr", "secretsmanager", "logs", "xray", "ec2", "elasticloadbalancing"]
  validation {
    condition     = length(setintersection(toset(var.more_interface_endpoints), toset(["kms", "sts", "s3"]))) == 0
    error_message = "S3, KMS and STS have endpoints of their own here"
  }
}

variable "zone_redundant" {
  description = "One NAT gateway and interface endpoint per zone (staging, production); false keeps one, in the first zone (dev, test)."
  type        = bool
  default     = true
}

variable "region" {
  description = "The region the network is built in; null is the provider's (a cell's disaster-recovery region names its own, ADR-0040)."
  type        = string
  default     = null
}

data "aws_region" "current" {
  region = var.region
}

locals {
  egress_zones        = var.zone_redundant ? var.availability_zones : [var.availability_zones[0]]
  endpoint_subnet_ids = [for az in local.egress_zones : aws_subnet.private[az].id]
  # STS's endpoint has subnets of its own, so that a network policy can
  # admit it and nothing else (erp-web's, ADR-0035 As built 2026-11-01:
  # the private subnets hold the nodes, the pods, the databases, the
  # cluster's own interfaces and every other endpoint). Each is a /28, the
  # smallest subnet AWS allows, in the VPC's fourth /20, which no other
  # subnet uses (the public subnets are the first three, the private ones
  # the upper three /18s). Derived from var.cidr alone, so the ranges are
  # known at plan time, before the endpoint's interfaces have addresses.
  sts_endpoint_cidrs = { for i, az in local.egress_zones : az => cidrsubnet(cidrsubnet(var.cidr, 4, 3), 8, i) }
}

resource "aws_vpc" "cell" {
  region               = var.region
  cidr_block           = var.cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = var.name }
}

resource "aws_subnet" "public" {
  region                  = var.region
  for_each                = { for i, az in var.availability_zones : az => i }
  vpc_id                  = aws_vpc.cell.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.cidr, 4, each.value)
  map_public_ip_on_launch = false
  tags                    = { Name = "${var.name}-public-${each.key}", "kubernetes.io/role/elb" = "1" }
}

resource "aws_subnet" "private" {
  region            = var.region
  for_each          = { for i, az in var.availability_zones : az => i }
  vpc_id            = aws_vpc.cell.id
  availability_zone = each.key
  cidr_block        = cidrsubnet(var.cidr, 2, each.value + 1)
  tags              = { Name = "${var.name}-private-${each.key}", "kubernetes.io/role/internal-elb" = "1" }
}

# No route table of their own: the endpoint's interfaces only answer, and
# the VPC's main table routes the VPC alone.
resource "aws_subnet" "sts_endpoint" {
  region            = var.region
  for_each          = local.sts_endpoint_cidrs
  vpc_id            = aws_vpc.cell.id
  availability_zone = each.key
  cidr_block        = each.value
  tags              = { Name = "${var.name}-sts-endpoint-${each.key}" }
}

resource "aws_internet_gateway" "cell" {
  region = var.region
  vpc_id = aws_vpc.cell.id
  tags   = { Name = var.name }
}

resource "aws_eip" "nat" {
  region   = var.region
  for_each = toset(local.egress_zones)
  domain   = "vpc"
  tags     = { Name = "${var.name}-nat-${each.key}" }
}

resource "aws_nat_gateway" "cell" {
  region        = var.region
  for_each      = toset(local.egress_zones)
  subnet_id     = aws_subnet.public[each.key].id
  allocation_id = aws_eip.nat[each.key].id
  tags          = { Name = "${var.name}-${each.key}" }
}

resource "aws_route_table" "public" {
  region = var.region
  vpc_id = aws_vpc.cell.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.cell.id
  }
  tags = { Name = "${var.name}-public" }
}

resource "aws_route_table_association" "public" {
  region         = var.region
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  region   = var.region
  for_each = aws_subnet.private
  vpc_id   = aws_vpc.cell.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.cell[var.zone_redundant ? each.key : local.egress_zones[0]].id
  }
  tags = { Name = "${var.name}-private-${each.key}" }
}

resource "aws_route_table_association" "private" {
  region         = var.region
  for_each       = aws_subnet.private
  subnet_id      = each.value.id
  route_table_id = aws_route_table.private[each.key].id
}

resource "aws_vpc_endpoint" "s3" {
  region            = var.region
  vpc_id            = aws_vpc.cell.id
  service_name      = "com.amazonaws.${data.aws_region.current.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [for table in aws_route_table.private : table.id]
  tags              = { Name = "${var.name}-s3" }
}

resource "aws_security_group" "endpoints" {
  region      = var.region
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
  region              = var.region
  vpc_id              = aws_vpc.cell.id
  service_name        = "com.amazonaws.${data.aws_region.current.region}.kms"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = local.endpoint_subnet_ids
  security_group_ids  = [aws_security_group.endpoints.id]
  tags                = { Name = "${var.name}-kms" }
}

resource "aws_vpc_endpoint" "sts" {
  region              = var.region
  vpc_id              = aws_vpc.cell.id
  service_name        = "com.amazonaws.${data.aws_region.current.region}.sts"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = [for az in local.egress_zones : aws_subnet.sts_endpoint[az].id]
  security_group_ids  = [aws_security_group.endpoints.id]
  tags                = { Name = "${var.name}-sts" }
}

resource "aws_vpc_endpoint" "interface" {
  region              = var.region
  for_each            = toset(var.more_interface_endpoints)
  vpc_id              = aws_vpc.cell.id
  service_name        = "com.amazonaws.${data.aws_region.current.region}.${each.key}"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = local.endpoint_subnet_ids
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

output "sts_endpoint_cidrs" {
  description = "The subnets STS's interface endpoint alone sits in, one /28 per zone of egress: the only addresses erp-web reaches in the VPC on 443 (the erp-global chart's awsEndpoints.stsCidrs)."
  value       = [for az in local.egress_zones : local.sts_endpoint_cidrs[az]]
}

output "cidr" {
  description = "The VPC's CIDR block."
  value       = var.cidr
}

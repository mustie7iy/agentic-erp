mock_provider "aws" {
  mock_data "aws_region" {
    defaults = { region = "eu-central-1" }
  }
}

variables {
  name               = "dev1"
  cidr               = "10.10.0.0/16"
  availability_zones = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
}

run "three_zones_with_private_and_public_subnets" {
  command = apply

  assert {
    condition     = length(aws_subnet.private) == 3 && length(aws_subnet.public) == 3 && length(aws_nat_gateway.cell) == 3
    error_message = "a private and a public subnet and a NAT gateway in each zone"
  }
  assert {
    condition     = length(setintersection(toset([for s in aws_subnet.private : s.cidr_block]), toset([for s in aws_subnet.public : s.cidr_block]))) == 0
    error_message = "subnets do not overlap"
  }
  assert {
    condition     = alltrue([for s in aws_subnet.public : !s.map_public_ip_on_launch])
    error_message = "nothing gets a public address by default"
  }
}

run "s3_kms_and_sts_are_never_proxied" {
  command = apply

  assert {
    condition     = aws_vpc_endpoint.s3.service_name == "com.amazonaws.eu-central-1.s3" && aws_vpc_endpoint.s3.vpc_endpoint_type == "Gateway"
    error_message = "S3 through a gateway endpoint on the private route tables"
  }
  assert {
    condition     = length(aws_vpc_endpoint.s3.route_table_ids) == 3
    error_message = "every private route table reaches S3 through the endpoint"
  }
  assert {
    condition     = aws_vpc_endpoint.kms.service_name == "com.amazonaws.eu-central-1.kms" && aws_vpc_endpoint.kms.private_dns_enabled
    error_message = "KMS through an interface endpoint with private DNS"
  }
  assert {
    condition     = aws_vpc_endpoint.sts.service_name == "com.amazonaws.eu-central-1.sts" && aws_vpc_endpoint.sts.private_dns_enabled
    error_message = "STS through an interface endpoint with private DNS"
  }
  assert {
    condition     = contains(keys(aws_vpc_endpoint.interface), "ecr.dkr") && contains(keys(aws_vpc_endpoint.interface), "secretsmanager") && contains(keys(aws_vpc_endpoint.interface), "logs") && contains(keys(aws_vpc_endpoint.interface), "xray")
    error_message = "images, secrets, logs and traces go through endpoints too"
  }
}

# erp-web's network policy admits STS's endpoint on 443 by these ranges
# alone (ADR-0035 As built 2026-11-01): the endpoint sits in them, and
# nothing else does.
run "sts_has_subnets_of_its_own" {
  command = apply

  assert {
    condition     = output.sts_endpoint_cidrs == ["10.10.48.0/28", "10.10.48.16/28", "10.10.48.32/28"]
    error_message = "a /28 per zone in the VPC's fourth /20, known from the VPC's range alone"
  }
  assert {
    condition     = [for az in var.availability_zones : aws_subnet.sts_endpoint[az].cidr_block] == output.sts_endpoint_cidrs && toset(aws_vpc_endpoint.sts.subnet_ids) == toset([for s in aws_subnet.sts_endpoint : s.id])
    error_message = "STS's endpoint sits in those subnets alone, one per zone"
  }
  assert {
    condition     = length(setintersection(toset(aws_vpc_endpoint.sts.subnet_ids), toset(concat([for s in aws_subnet.private : s.id], [for s in aws_subnet.public : s.id])))) == 0
    error_message = "STS's endpoint sits in no subnet the nodes, databases, load balancers or other endpoints use"
  }
  assert {
    condition     = [for s in aws_subnet.public : s.cidr_block] == ["10.10.0.0/20", "10.10.16.0/20", "10.10.32.0/20"] && [for s in aws_subnet.private : s.cidr_block] == ["10.10.64.0/18", "10.10.128.0/18", "10.10.192.0/18"]
    error_message = "the public and private subnets leave the fourth /20 (10.10.48.0/20) to STS's endpoint"
  }
  assert {
    condition     = toset(aws_vpc_endpoint.kms.subnet_ids) == toset([for s in aws_subnet.private : s.id]) && alltrue([for e in aws_vpc_endpoint.interface : toset(e.subnet_ids) == toset([for s in aws_subnet.private : s.id])])
    error_message = "the other interface endpoints stay in the private subnets"
  }
}

run "kms_cannot_be_moved_to_the_optional_list" {
  command = plan
  variables {
    more_interface_endpoints = ["kms"]
  }
  expect_failures = [var.more_interface_endpoints]
}

run "a_cell_spans_three_zones" {
  command = plan
  variables {
    availability_zones = ["eu-central-1a", "eu-central-1b"]
  }
  expect_failures = [var.availability_zones]
}

run "dev_and_test_keep_one_zone_of_egress_and_every_endpoint" {
  command = apply
  variables {
    zone_redundant = false
  }

  assert {
    condition     = keys(aws_nat_gateway.cell) == ["eu-central-1a"] && length(aws_eip.nat) == 1
    error_message = "one NAT gateway, in the first zone"
  }
  assert {
    condition     = alltrue([for t in aws_route_table.private : one(t.route).nat_gateway_id == aws_nat_gateway.cell["eu-central-1a"].id])
    error_message = "every private subnet leaves through that NAT gateway"
  }
  assert {
    condition     = length(aws_vpc_endpoint.kms.subnet_ids) == 1 && length(aws_vpc_endpoint.sts.subnet_ids) == 1 && alltrue([for e in aws_vpc_endpoint.interface : length(e.subnet_ids) == 1])
    error_message = "the interface endpoints sit in one zone"
  }
  assert {
    condition     = output.sts_endpoint_cidrs == ["10.10.48.0/28"] && keys(aws_subnet.sts_endpoint) == ["eu-central-1a"]
    error_message = "STS's endpoint has one subnet of its own, in the first zone"
  }
  assert {
    condition     = aws_vpc_endpoint.kms.private_dns_enabled && aws_vpc_endpoint.sts.private_dns_enabled && length(aws_vpc_endpoint.s3.route_table_ids) == 3
    error_message = "S3, KMS and STS are still never proxied"
  }
}

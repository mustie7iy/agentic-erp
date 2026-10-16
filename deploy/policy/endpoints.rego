# Providers are never proxied (T38b, ADR-0035): a VPC's workloads reach S3,
# KMS and STS through VPC endpoints (PrivateLink, a gateway for S3), never
# through a NAT gateway and the internet. Every aws_vpc declared in a
# directory needs, in the same directory, an aws_vpc_endpoint for each of
# the three services, whatever region the service name is written for.
package main

import rego.v1

required_endpoints := {"s3", "kms", "sts"}

deny contains msg if {
	some ["aws_vpc", name, _] in resources
	some service in required_endpoints
	not endpoint_for(name, service)
	msg := sprintf("aws_vpc.%s: needs an aws_vpc_endpoint for %s (providers are never proxied)", [name, service])
}

endpoint_for(vpc, service) if {
	some ["aws_vpc_endpoint", _, endpoint] in resources
	referenced(endpoint.vpc_id, "aws_vpc", vpc)
	endswith(endpoint.service_name, sprintf(".%s", [service]))
}

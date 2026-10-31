# Unit tests of endpoints.rego (conftest verify).
package main

import rego.v1

vpc_with(services) := file(object.union({"aws_vpc": {"cell": [{"cidr_block": "10.0.0.0/16"}]}}, {"aws_vpc_endpoint": {s: [{
	"vpc_id": "${aws_vpc.cell.id}",
	"service_name": sprintf("com.amazonaws.${data.aws_region.current.region}.%s", [s]),
}] |
	some s in services
}}))

test_a_vpc_with_the_three_endpoints_passes if {
	count(deny) == 0 with input as vpc_with(["s3", "kms", "sts", "ecr.api"])
}

test_each_missing_endpoint_is_refused if {
	refused := deny with input as vpc_with(["s3"])
	refused["aws_vpc.cell: needs an aws_vpc_endpoint for kms (providers are never proxied)"]
	refused["aws_vpc.cell: needs an aws_vpc_endpoint for sts (providers are never proxied)"]
	not refused["aws_vpc.cell: needs an aws_vpc_endpoint for s3 (providers are never proxied)"]
}

test_an_endpoint_of_another_vpc_does_not_count if {
	other := {
		"aws_vpc": {"cell": [{"cidr_block": "10.0.0.0/16"}], "other": [{"cidr_block": "10.1.0.0/16"}]},
		"aws_vpc_endpoint": {s: [{"vpc_id": "${aws_vpc.other.id}", "service_name": sprintf("com.amazonaws.eu-central-1.%s", [s])}] | some s in ["s3", "kms", "sts"]},
	}
	deny["aws_vpc.cell: needs an aws_vpc_endpoint for s3 (providers are never proxied)"] with input as file(other)
}

test_kms_fips_is_not_kms if {
	deny["aws_vpc.cell: needs an aws_vpc_endpoint for kms (providers are never proxied)"] with input as vpc_with(["s3", "kms-fips", "sts"])
}

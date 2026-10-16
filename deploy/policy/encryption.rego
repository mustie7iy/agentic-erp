# Encryption at rest for the infrastructure (ADR-0035, from T38): conftest
# applies it to the Terraform of every module and environment root, parsed
# as HCL with all of a directory's files combined (scripts/deploy-checks.sh).
# Each rule refuses one way of leaving data unencrypted, or encrypted only
# with a key the platform does not control where it should.
package main

import rego.v1

# resources yields [type, name, body] for every resource of every file.
resources contains [type, name, body] if {
	some file in input
	some type, named in file.contents.resource
	some name, bodies in named
	some body in bodies
}

# referenced is true when the expression expr names resource type.name:
# an attribute of it (.id, .bucket) or one instance (for_each, count), and
# not a data source or another resource whose name ends the same.
referenced(expr, type, name) if regex.match(sprintf(`(^|[^A-Za-z0-9_.])%s\.%s[.\[]`, [type, name]), expr)

deny contains msg if {
	some [type, name, body] in resources
	type == "aws_kms_key"
	not asymmetric(body)
	object.get(body, "enable_key_rotation", false) != true
	msg := sprintf("aws_kms_key.%s: a symmetric key must rotate (enable_key_rotation = true)", [name])
}

# asymmetric is true for a key spec written out as an asymmetric one; a
# spec from an expression is not known here and counts as symmetric.
asymmetric(body) if {
	spec := object.get(body, "customer_master_key_spec", "SYMMETRIC_DEFAULT")
	not contains(spec, "${")
	spec != "SYMMETRIC_DEFAULT"
}

deny contains msg if {
	some ["aws_s3_bucket", name, _] in resources
	not bucket_kms_encrypted(name)
	msg := sprintf("aws_s3_bucket.%s: needs aws_s3_bucket_server_side_encryption_configuration with sse_algorithm aws:kms and a kms_master_key_id", [name])
}

bucket_kms_encrypted(name) if {
	some ["aws_s3_bucket_server_side_encryption_configuration", _, config] in resources
	referenced(config.bucket, "aws_s3_bucket", name)
	some rule in config.rule
	some by_default in rule.apply_server_side_encryption_by_default
	by_default.sse_algorithm == "aws:kms"
	object.get(by_default, "kms_master_key_id", "") != ""
}

deny contains msg if {
	some ["aws_s3_bucket", name, _] in resources
	not bucket_public_access_blocked(name)
	msg := sprintf("aws_s3_bucket.%s: needs aws_s3_bucket_public_access_block with all four settings true", [name])
}

bucket_public_access_blocked(name) if {
	some ["aws_s3_bucket_public_access_block", _, block] in resources
	referenced(block.bucket, "aws_s3_bucket", name)
	block.block_public_acls == true
	block.block_public_policy == true
	block.ignore_public_acls == true
	block.restrict_public_buckets == true
}

deny contains msg if {
	some [type, name, body] in resources
	type in {"aws_ecr_repository", "aws_ecr_repository_creation_template"}
	not kms_encrypted_repository(body)
	msg := sprintf("%s.%s: needs encryption_configuration with encryption_type KMS", [type, name])
}

kms_encrypted_repository(body) if {
	some config in body.encryption_configuration
	config.encryption_type == "KMS"
}

deny contains msg if {
	some [type, name, body] in resources
	type in {"aws_db_instance", "aws_rds_cluster"}
	object.get(body, "storage_encrypted", false) != true
	msg := sprintf("%s.%s: storage_encrypted must be true", [type, name])
}

deny contains msg if {
	some ["aws_ebs_volume", name, body] in resources
	object.get(body, "encrypted", false) != true
	msg := sprintf("aws_ebs_volume.%s: encrypted must be true", [name])
}

deny contains msg if {
	some ["aws_eks_cluster", name, body] in resources
	not eks_secrets_encrypted(body)
	msg := sprintf("aws_eks_cluster.%s: needs encryption_config for secrets with a KMS key", [name])
}

eks_secrets_encrypted(body) if {
	some config in body.encryption_config
	"secrets" in config.resources
	some provider in config.provider
	object.get(provider, "key_arn", "") != ""
}

deny contains msg if {
	some ["aws_dynamodb_table", name, body] in resources
	not dynamodb_kms_encrypted(body)
	msg := sprintf("aws_dynamodb_table.%s: needs server_side_encryption enabled (encryption with a KMS key)", [name])
}

dynamodb_kms_encrypted(body) if {
	some sse in body.server_side_encryption
	sse.enabled == true
}

# Unit tests of encryption.rego (conftest verify). Inputs have the shape
# conftest gives combined HCL: a list of files, each with its parsed
# contents; blocks are lists, references are "${...}" strings.
package main

import rego.v1

file(resource) := [{"path": "main.tf", "contents": {"resource": resource}}]

encrypted_bucket := {
	"aws_s3_bucket": {"state": [{"bucket": "erp-state"}]},
	"aws_s3_bucket_server_side_encryption_configuration": {"state": [{
		"bucket": "${aws_s3_bucket.state.id}",
		"rule": [{"apply_server_side_encryption_by_default": [{
			"sse_algorithm": "aws:kms",
			"kms_master_key_id": "${aws_kms_key.state.arn}",
		}]}],
	}]},
	"aws_s3_bucket_public_access_block": {"state": [{
		"bucket": "${aws_s3_bucket.state.id}",
		"block_public_acls": true,
		"block_public_policy": true,
		"ignore_public_acls": true,
		"restrict_public_buckets": true,
	}]},
	"aws_kms_key": {"state": [{"enable_key_rotation": true}]},
}

test_encrypted_bucket_passes if {
	count(deny) == 0 with input as file(encrypted_bucket)
}

test_bucket_without_encryption if {
	resource := object.remove(encrypted_bucket, ["aws_s3_bucket_server_side_encryption_configuration"])
	deny["aws_s3_bucket.state: needs aws_s3_bucket_server_side_encryption_configuration with sse_algorithm aws:kms and a kms_master_key_id"] with input as file(resource)
}

test_bucket_encrypted_for_another_bucket if {
	config := encrypted_bucket.aws_s3_bucket_server_side_encryption_configuration.state[0]
	resource := object.union(encrypted_bucket, {"aws_s3_bucket_server_side_encryption_configuration": {"state": [object.union(config, {"bucket": "${aws_s3_bucket.logs.id}"})]}})
	deny["aws_s3_bucket.state: needs aws_s3_bucket_server_side_encryption_configuration with sse_algorithm aws:kms and a kms_master_key_id"] with input as file(resource)
}

test_bucket_encryption_across_files if {
	split_files := [
		{"path": "bucket.tf", "contents": {"resource": object.remove(encrypted_bucket, ["aws_s3_bucket_server_side_encryption_configuration"])}},
		{"path": "sse.tf", "contents": {"resource": object.filter(encrypted_bucket, ["aws_s3_bucket_server_side_encryption_configuration"])}},
	]
	count(deny) == 0 with input as split_files
}

test_bucket_without_public_access_block if {
	resource := object.remove(encrypted_bucket, ["aws_s3_bucket_public_access_block"])
	deny["aws_s3_bucket.state: needs aws_s3_bucket_public_access_block with all four settings true"] with input as file(resource)
}

test_unrotated_symmetric_key if {
	deny["aws_kms_key.k: a symmetric key must rotate (enable_key_rotation = true)"] with input as file({"aws_kms_key": {"k": [{}]}})
}

test_asymmetric_signing_key_needs_no_rotation if {
	count(deny) == 0 with input as file({"aws_kms_key": {"signing": [{"customer_master_key_spec": "ECC_NIST_P256", "key_usage": "SIGN_VERIFY"}]}})
}

test_ecr_repository_kms if {
	count(deny) == 0 with input as file({"aws_ecr_repository": {"r": [{"encryption_configuration": [{"encryption_type": "KMS"}]}]}})
	deny["aws_ecr_repository.r: needs encryption_configuration with encryption_type KMS"] with input as file({"aws_ecr_repository": {"r": [{}]}})
}

test_ecr_replication_template_kms if {
	deny["aws_ecr_repository_creation_template.t: needs encryption_configuration with encryption_type KMS"] with input as file({"aws_ecr_repository_creation_template": {"t": [{"encryption_configuration": [{"encryption_type": "AES256"}]}]}})
}

test_databases_and_volumes if {
	deny["aws_db_instance.d: storage_encrypted must be true"] with input as file({"aws_db_instance": {"d": [{}]}})
	deny["aws_rds_cluster.c: storage_encrypted must be true"] with input as file({"aws_rds_cluster": {"c": [{"storage_encrypted": false}]}})
	deny["aws_ebs_volume.v: encrypted must be true"] with input as file({"aws_ebs_volume": {"v": [{}]}})
	count(deny) == 0 with input as file({"aws_db_instance": {"d": [{"storage_encrypted": true}]}, "aws_ebs_volume": {"v": [{"encrypted": true}]}})
}

test_eks_secrets_encryption if {
	deny["aws_eks_cluster.e: needs encryption_config for secrets with a KMS key"] with input as file({"aws_eks_cluster": {"e": [{}]}})
	count(deny) == 0 with input as file({"aws_eks_cluster": {"e": [{"encryption_config": [{"resources": ["secrets"], "provider": [{"key_arn": "${aws_kms_key.eks.arn}"}]}]}]}})
}

test_dynamodb_encryption if {
	deny["aws_dynamodb_table.t: needs server_side_encryption enabled (encryption with a KMS key)"] with input as file({"aws_dynamodb_table": {"t": [{}]}})
	count(deny) == 0 with input as file({"aws_dynamodb_table": {"t": [{"server_side_encryption": [{"enabled": true}]}]}})
}

test_bucket_encrypted_without_a_key if {
	config := encrypted_bucket.aws_s3_bucket_server_side_encryption_configuration.state[0]
	no_key := object.union(config, {"rule": [{"apply_server_side_encryption_by_default": [{"sse_algorithm": "aws:kms"}]}]})
	resource := object.union(encrypted_bucket, {"aws_s3_bucket_server_side_encryption_configuration": {"state": [no_key]}})
	deny["aws_s3_bucket.state: needs aws_s3_bucket_server_side_encryption_configuration with sse_algorithm aws:kms and a kms_master_key_id"] with input as file(resource)
}

test_bucket_per_instance_reference if {
	instances := {
		"aws_s3_bucket": {"state": [{"for_each": "${var.buckets}", "bucket": "${each.key}"}]},
		"aws_s3_bucket_server_side_encryption_configuration": {"state": [{
			"for_each": "${var.buckets}",
			"bucket": "${aws_s3_bucket.state[each.key].id}",
			"rule": [{"apply_server_side_encryption_by_default": [{"sse_algorithm": "aws:kms", "kms_master_key_id": "${aws_kms_key.state.arn}"}]}],
		}]},
		"aws_s3_bucket_public_access_block": {"state": [object.union(encrypted_bucket.aws_s3_bucket_public_access_block.state[0], {"bucket": "${aws_s3_bucket.state[each.key].id}"})]},
	}
	count(deny) == 0 with input as file(instances)
}

test_a_data_source_does_not_encrypt_a_resource if {
	config := encrypted_bucket.aws_s3_bucket_server_side_encryption_configuration.state[0]
	resource := object.union(encrypted_bucket, {"aws_s3_bucket_server_side_encryption_configuration": {"state": [object.union(config, {"bucket": "${data.aws_s3_bucket.state.id}"})]}})
	deny["aws_s3_bucket.state: needs aws_s3_bucket_server_side_encryption_configuration with sse_algorithm aws:kms and a kms_master_key_id"] with input as file(resource)
}

test_a_key_spec_from_a_variable_must_rotate if {
	deny["aws_kms_key.k: a symmetric key must rotate (enable_key_rotation = true)"] with input as file({"aws_kms_key": {"k": [{"customer_master_key_spec": "${var.spec}"}]}})
}

test_cell_resources if {
	deny["aws_launch_template.t: every EBS volume it maps must set encrypted = true"] with input as file({"aws_launch_template": {"t": [{"block_device_mappings": [{"ebs": [{"volume_size": 20}]}]}]}})
	count(deny) == 0 with input as file({"aws_launch_template": {"t": [{"block_device_mappings": [{"ebs": [{"encrypted": true}]}]}]}})
	deny["aws_cloudwatch_log_group.l: needs kms_key_id (a key of the platform's)"] with input as file({"aws_cloudwatch_log_group": {"l": [{}]}})
	deny["aws_secretsmanager_secret.s: needs kms_key_id (a key of the platform's)"] with input as file({"aws_secretsmanager_secret": {"s": [{}]}})
	count(deny) == 0 with input as file({"aws_secretsmanager_secret": {"s": [{"kms_key_id": "${aws_kms_key.secrets.arn}"}]}})
	deny["aws_ebs_encryption_by_default.d: must stay enabled"] with input as file({"aws_ebs_encryption_by_default": {"d": [{"enabled": false}]}})
	count(deny) == 0 with input as file({"aws_ebs_encryption_by_default": {"d": [{}]}})
	deny["aws_efs_file_system.f: encrypted must be true"] with input as file({"aws_efs_file_system": {"f": [{}]}})
	deny["aws_elasticache_replication_group.r: at_rest_encryption_enabled must be true"] with input as file({"aws_elasticache_replication_group": {"r": [{}]}})
}

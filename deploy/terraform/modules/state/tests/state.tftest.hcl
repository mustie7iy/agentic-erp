mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
}

variables {
  bucket = "erp-terraform-state-test"
}

run "state_is_versioned_encrypted_private_and_tls_only" {
  command = apply

  assert {
    condition     = aws_s3_bucket_versioning.state.versioning_configuration[0].status == "Enabled"
    error_message = "the state bucket must be versioned"
  }
  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.state.rule).apply_server_side_encryption_by_default).sse_algorithm == "aws:kms"
    error_message = "the state must be encrypted with KMS"
  }
  assert {
    condition     = aws_kms_key.state.enable_key_rotation
    error_message = "the state key must rotate"
  }
  assert {
    condition     = jsondecode(aws_s3_bucket_policy.state.policy).Statement[0].Condition.Bool["aws:SecureTransport"] == "false" && jsondecode(aws_s3_bucket_policy.state.policy).Statement[0].Effect == "Deny"
    error_message = "the bucket must refuse requests without TLS"
  }
  assert {
    condition     = aws_s3_bucket_public_access_block.state.restrict_public_buckets && aws_s3_bucket_public_access_block.state.block_public_policy
    error_message = "the bucket must never be public"
  }
  assert {
    condition     = contains(jsondecode(aws_kms_key.state.policy).Statement[0].Principal.AWS, "arn:aws:iam::111111111111:root")
    error_message = "the account root must administer the state key"
  }
}

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

run "writes_must_use_the_state_key" {
  command = apply

  assert {
    condition = length([
      for s in jsondecode(aws_s3_bucket_policy.state.policy).Statement : s
      if s.Effect == "Deny" && s.Action == "s3:PutObject" && try(s.Condition.StringNotEqualsIfExists["s3:x-amz-server-side-encryption"], "") == "aws:kms"
    ]) == 1
    error_message = "a write asking for SSE-S3 (the backend without kms_key_id) is refused"
  }
  assert {
    condition = length([
      for s in jsondecode(aws_s3_bucket_policy.state.policy).Statement : s
      if s.Effect == "Deny" && try(s.Condition.StringNotEqualsIfExists["s3:x-amz-server-side-encryption-aws-kms-key-id"], "") == aws_kms_key.state.arn
    ]) == 1
    error_message = "a write naming another KMS key is refused"
  }
  assert {
    condition     = one(aws_s3_bucket_lifecycle_configuration.state.rule).noncurrent_version_expiration[0].noncurrent_days == 365
    error_message = "superseded state versions expire after a year"
  }
}

run "another_accounts_apply_role_may_use_the_state" {
  command = apply
  variables {
    state_role_arns = ["arn:aws:iam::999999999999:role/organization-apply"]
  }

  assert {
    condition     = [for s in jsondecode(aws_s3_bucket_policy.state.policy).Statement : s.Principal.AWS if s.Sid == "OtherAccountsUseState"][0] == ["arn:aws:iam::999999999999:role/organization-apply"]
    error_message = "the management account's apply role may read and write state"
  }
  assert {
    condition     = contains([for s in jsondecode(aws_kms_key.state.policy).Statement : s.Action if s.Sid == "OtherAccountsUseState"][0], "kms:Decrypt")
    error_message = "and use the state key"
  }
}

run "a_whole_account_is_refused" {
  command = plan
  variables {
    state_role_arns = ["arn:aws:iam::999999999999:root"]
  }
  expect_failures = [var.state_role_arns]
}

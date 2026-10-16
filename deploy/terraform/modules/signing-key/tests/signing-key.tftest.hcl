mock_provider "aws" {
  mock_resource "aws_kms_key" {
    defaults = { arn = "arn:aws:kms:eu-central-1:111111111111:key/0000" }
  }
}

variables {
  admin_role_arns    = ["arn:aws:iam::111111111111:role/admin"]
  signer_role_arns   = ["arn:aws:iam::111111111111:role/erp-release"]
  verifier_role_arns = ["arn:aws:iam::111111111111:role/erp-delivery"]
}

run "an_asymmetric_key_with_a_least_privilege_policy" {
  command = apply

  assert {
    condition     = aws_kms_key.signing.customer_master_key_spec == "ECC_NIST_P256" && aws_kms_key.signing.key_usage == "SIGN_VERIFY"
    error_message = "the signing key is ECC_NIST_P256 for SIGN_VERIFY (ADR-0032)"
  }
  assert {
    condition     = output.cosign_key == "awskms:///arn:aws:kms:eu-central-1:111111111111:key/0000"
    error_message = "the cosign reference is awskms:/// and the key's ARN"
  }
  assert {
    condition = alltrue([
      for s in jsondecode(aws_kms_key.signing.policy).Statement :
      !contains(flatten([s.Action]), "kms:Sign") || jsonencode(s.Principal.AWS) == jsonencode(var.signer_role_arns)
    ])
    error_message = "only the signer may sign"
  }
  assert {
    condition = alltrue([
      for s in jsondecode(aws_kms_key.signing.policy).Statement :
      s.Sid != "Administer" || !contains(s.Action, "kms:Sign")
    ])
    error_message = "administrators must not sign"
  }
  assert {
    condition     = [for s in jsondecode(aws_kms_key.signing.policy).Statement : s.Action if s.Sid == "Verify"][0] == ["kms:GetPublicKey", "kms:DescribeKey"]
    error_message = "verifiers only read the public key"
  }
}

run "no_verifiers_means_no_verify_statement" {
  command = apply
  variables {
    verifier_role_arns = []
  }
  assert {
    condition     = length([for s in jsondecode(aws_kms_key.signing.policy).Statement : s if s.Sid == "Verify"]) == 0
    error_message = "an empty verifier list must not leave a statement without principals"
  }
}

run "a_key_without_administrator_is_refused" {
  command = plan
  variables {
    admin_role_arns = []
  }
  expect_failures = [var.admin_role_arns]
}

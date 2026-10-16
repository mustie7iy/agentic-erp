mock_provider "aws" {
  mock_resource "aws_iam_openid_connect_provider" {
    defaults = { arn = "arn:aws:iam::111111111111:oidc-provider/token.actions.githubusercontent.com" }
  }
}

variables {
  repository      = "mustie7iy/agentic-erp"
  signing_key_arn = "arn:aws:kms:eu-central-1:111111111111:key/0000"
  repository_arns = ["arn:aws:ecr:eu-central-1:111111111111:repository/erp-server"]
}

run "each_role_trusts_exactly_its_repository_and_environment" {
  command = apply

  assert {
    condition     = jsondecode(aws_iam_role.release.assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:sub"] == "repo:mustie7iy/agentic-erp:environment:release"
    error_message = "the release role trusts the release environment of this repository only"
  }
  assert {
    condition     = jsondecode(aws_iam_role.release.assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:aud"] == "sts.amazonaws.com"
    error_message = "the release role requires the STS audience"
  }
  assert {
    condition     = length(jsondecode(aws_iam_role.release.assume_role_policy).Statement) == 1
    error_message = "nothing else may assume the release role"
  }
  assert {
    condition     = jsondecode(aws_iam_role.delivery.assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:sub"] == "repo:mustie7iy/agentic-erp:environment:delivery"
    error_message = "the delivery role trusts the delivery environment of this repository only"
  }
}

run "release_signs_and_pushes_delivery_only_reads" {
  command = apply

  assert {
    condition     = contains([for s in jsondecode(aws_iam_role_policy.release.policy).Statement : s.Action if s.Sid == "Sign"][0], "kms:Sign")
    error_message = "the release role signs"
  }
  assert {
    condition     = [for s in jsondecode(aws_iam_role_policy.release.policy).Statement : s.Resource if s.Sid == "Sign"][0] == var.signing_key_arn
    error_message = "the release role signs with that key only"
  }
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.delivery.policy).Statement :
      !contains(flatten([s.Action]), "kms:Sign") && !contains(flatten([s.Action]), "ecr:PutImage")
    ])
    error_message = "the delivery role neither signs nor pushes"
  }
}

run "argo_cd_may_be_added_to_delivery" {
  command = apply
  variables {
    delivery_principal_arns = ["arn:aws:iam::222222222222:role/argocd"]
  }
  assert {
    condition     = jsondecode(aws_iam_role.delivery.assume_role_policy).Statement[1].Principal.AWS == ["arn:aws:iam::222222222222:role/argocd"]
    error_message = "a further delivery principal is trusted"
  }
}

run "a_whole_account_may_not_assume_delivery" {
  command = plan
  variables {
    delivery_principal_arns = ["arn:aws:iam::222222222222:root"]
  }
  expect_failures = [var.delivery_principal_arns]
}

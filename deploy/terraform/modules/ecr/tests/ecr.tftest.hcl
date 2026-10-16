mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "111111111111" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
}

variables {
  replication_regions = ["eu-west-1", "us-east-1"]
  pull_account_ids    = ["222222222222", "333333333333"]
}

run "one_immutable_encrypted_scanned_repository_per_binary" {
  command = apply

  assert {
    condition     = toset(keys(aws_ecr_repository.this)) == toset(["erp-server", "erp-router", "erp"])
    error_message = "one repository per binary"
  }
  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.image_tag_mutability == "IMMUTABLE_WITH_EXCLUSION" && r.image_tag_mutability_exclusion_filter[0].filter == "sha256-*"])
    error_message = "release tags are immutable; only cosign's sha256-* tags may change"
  }
  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.encryption_configuration[0].encryption_type == "KMS" && r.image_scanning_configuration[0].scan_on_push])
    error_message = "repositories are KMS-encrypted and scanned on push"
  }
}

run "the_lifecycle_never_expires_untagged_signatures" {
  command = apply

  assert {
    condition     = length(aws_ecr_lifecycle_policy.this) == 3 && alltrue([for p in aws_ecr_lifecycle_policy.this : length(jsondecode(p.policy).rules) == 1])
    error_message = "every repository has its one lifecycle rule"
  }
  assert {
    condition = alltrue([
      for p in aws_ecr_lifecycle_policy.this : alltrue([
        for rule in jsondecode(p.policy).rules : rule.selection.tagStatus == "tagged" && rule.selection.tagPrefixList == ["v"]
      ])
    ])
    error_message = "only release tags may expire"
  }
}

run "replicated_to_every_cell_region_and_pulled_by_the_environments" {
  command = apply

  assert {
    condition     = [for d in aws_ecr_replication_configuration.this[0].replication_configuration[0].rule[0].destination : d.region] == ["eu-west-1", "us-east-1"]
    error_message = "replicated to every given region"
  }
  assert {
    condition     = toset([for t in aws_ecr_repository_creation_template.replicas : t.region]) == toset(["eu-west-1", "us-east-1"])
    error_message = "a creation template in every destination region, where the replicas are created"
  }
  assert {
    condition     = alltrue([for t in aws_ecr_repository_creation_template.replicas : t.encryption_configuration[0].encryption_type == "KMS" && t.repository_policy != null])
    error_message = "replicas are KMS-encrypted and pulled by the environment accounts"
  }
  assert {
    condition     = jsondecode(aws_ecr_repository_policy.pull["erp-server"].policy).Statement[0].Principal.AWS == ["arn:aws:iam::222222222222:root", "arn:aws:iam::333333333333:root"]
    error_message = "the environment accounts may pull"
  }
  assert {
    condition     = !contains(jsondecode(aws_ecr_repository_policy.pull["erp-server"].policy).Statement[0].Action, "ecr:PutImage")
    error_message = "the environment accounts may not push"
  }
}

run "no_replication_or_pull_policy_without_regions_or_accounts" {
  command = apply
  variables {
    replication_regions = []
    pull_account_ids    = []
  }
  assert {
    condition     = length(aws_ecr_replication_configuration.this) == 0 && length(aws_ecr_repository_policy.pull) == 0
    error_message = "nothing to replicate or share"
  }
}

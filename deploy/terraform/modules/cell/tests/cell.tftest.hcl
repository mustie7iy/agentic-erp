mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "222222222222" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_data "aws_region" {
    defaults = { region = "eu-central-1" }
  }
  mock_data "aws_rds_engine_version" {
    defaults = { version_actual = "17.6" }
  }
  mock_resource "aws_kms_key" {
    defaults = { arn = "arn:aws:kms:eu-central-1:222222222222:key/0000" }
  }
  mock_resource "aws_eks_cluster" {
    defaults = {
      arn      = "arn:aws:eks:eu-central-1:222222222222:cluster/erp-dev-1"
      identity = [{ oidc = [{ issuer = "https://oidc.eks.eu-central-1.amazonaws.com/id/ABC" }] }]
    }
  }
  mock_resource "aws_iam_openid_connect_provider" {
    defaults = {
      arn = "arn:aws:iam::222222222222:oidc-provider/oidc.eks.eu-central-1.amazonaws.com/id/ABC"
      url = "https://oidc.eks.eu-central-1.amazonaws.com/id/ABC"
    }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::222222222222:role/mock" }
  }
  mock_resource "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::erp-dev-1-objects-x" }
  }
  mock_resource "aws_launch_template" {
    defaults = { id = "lt-0123456789abcdef0", latest_version = 1 }
  }
  mock_resource "aws_secretsmanager_secret" {
    defaults = { arn = "arn:aws:secretsmanager:eu-central-1:222222222222:secret:erp/dev-1/x" }
  }
}

variables {
  name                 = "dev-1"
  cidr                 = "10.10.0.0/16"
  availability_zones   = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
  admin_role_arns      = ["arn:aws:iam::222222222222:role/platform-admin"]
  subordinate_ca_arn   = "arn:aws:acm-pca:eu-central-1:222222222222:certificate-authority/1111"
  autoscaling_role_arn = "arn:aws:iam::222222222222:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling"
}

run "the_database_requires_tls_1_3_and_survives_a_zone" {
  command = apply

  assert {
    condition     = { for p in aws_rds_cluster_parameter_group.cell.parameter : p.name => p.value }["ssl_min_protocol_version"] == "TLSv1.3"
    error_message = "PostgreSQL accepts TLS 1.3 and nothing older (T38)"
  }
  assert {
    condition     = { for p in aws_rds_cluster_parameter_group.cell.parameter : p.name => p.value }["rds.force_ssl"] == "1"
    error_message = "PostgreSQL refuses clients without TLS"
  }
  assert {
    condition     = aws_rds_cluster.cell.db_cluster_instance_class != null && aws_rds_cluster.cell.engine == "postgres" && aws_rds_cluster.cell.engine_version == "17.6"
    error_message = "a Multi-AZ DB cluster of PostgreSQL 17"
  }
  assert {
    condition     = aws_rds_cluster.cell.storage_encrypted && aws_rds_cluster.cell.deletion_protection && aws_rds_cluster.cell.backup_retention_period >= 14
    error_message = "encrypted, protected from deletion, two weeks of point-in-time recovery"
  }
  assert {
    condition     = aws_rds_cluster.cell.manage_master_user_password
    error_message = "the master password lives in Secrets Manager, not in Terraform"
  }
}

run "the_cluster_is_private_and_encrypted" {
  command = apply

  assert {
    condition     = !aws_eks_cluster.cell.vpc_config[0].endpoint_public_access && aws_eks_cluster.cell.vpc_config[0].endpoint_private_access
    error_message = "without public_api_cidrs the Kubernetes API is private"
  }
  assert {
    condition     = aws_eks_cluster.cell.encryption_config[0].resources == toset(["secrets"])
    error_message = "Kubernetes secrets are encrypted with the cell's key"
  }
  assert {
    condition     = !aws_eks_cluster.cell.access_config[0].bootstrap_cluster_creator_admin_permissions
    error_message = "whoever creates the cluster gets no implicit access"
  }
  assert {
    condition     = aws_launch_template.nodes.metadata_options[0].http_tokens == "required" && aws_launch_template.nodes.metadata_options[0].http_put_response_hop_limit == 1
    error_message = "pods cannot reach the nodes' instance metadata; they use IRSA"
  }
  assert {
    condition     = aws_launch_template.nodes.block_device_mappings[0].ebs[0].encrypted == "true"
    error_message = "node volumes are encrypted"
  }
  assert {
    condition     = aws_ebs_encryption_by_default.cell[0].enabled
    error_message = "every new EBS volume of the account is encrypted"
  }
}

run "each_workload_role_trusts_its_service_account_only" {
  command = apply

  assert {
    condition     = jsondecode(aws_iam_role.workload["server"].assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.eu-central-1.amazonaws.com/id/ABC:sub"] == "system:serviceaccount:erp:erp-server"
    error_message = "the server's role trusts the erp/erp-server service account only"
  }
  assert {
    condition     = jsondecode(aws_iam_role.workload["cert_manager"].assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.eu-central-1.amazonaws.com/id/ABC:sub"] == "system:serviceaccount:cert-manager:aws-privateca-issuer"
    error_message = "the issuer's role trusts cert-manager's issuer only"
  }
  assert {
    condition     = jsondecode(aws_iam_role_policy.workload["cert_manager"].policy).Statement[0].Resource == var.subordinate_ca_arn
    error_message = "certificates come from the environment's subordinate CA only"
  }
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.workload["external_secrets"].policy).Statement :
      !contains(flatten([s.Action]), "secretsmanager:PutSecretValue")
    ])
    error_message = "the External Secrets Operator only reads"
  }
}

run "only_the_server_unwraps_tenant_keys" {
  command = apply

  assert {
    condition = alltrue([
      for st in jsondecode(aws_kms_key.fields.policy).Statement :
      !anytrue([for a in flatten([st.Action]) : contains(["kms:*", "kms:Decrypt", "kms:Encrypt", "kms:ReEncrypt*", "kms:CreateGrant"], a)])
      if st.Sid != "ServerWrapsTenantKeys"
    ])
    error_message = "administrators and the account manage the field key but cannot use it (ADR-0019)"
  }
  assert {
    condition     = [for st in jsondecode(aws_kms_key.fields.policy).Statement : st.Principal.AWS if st.Sid == "ServerWrapsTenantKeys"][0] == aws_iam_role.workload["server"].arn
    error_message = "the server's role wraps and unwraps tenant keys"
  }
  assert {
    condition     = [for st in jsondecode(aws_kms_key.fields.policy).Statement : st.Condition if st.Sid == "ServerWrapsTenantKeys"][0]["ForAllValues:StringEquals"]["kms:EncryptionContextKeys"] == ["tenant"]
    error_message = "only with a tenant encryption context"
  }
}

run "the_volume_key_names_the_auto_scaling_role_it_is_given" {
  command = apply

  assert {
    condition     = [for st in jsondecode(aws_kms_key.cluster.policy).Statement : st.Condition if st.Sid == "AutoScalingVolumeGrants"][0].Bool["kms:GrantIsForAWSResource"] == "true"
    error_message = "Auto Scaling creates grants for AWS resources only"
  }
}

run "the_issuer_asks_for_end_entity_certificates_only" {
  command = apply

  assert {
    condition     = jsondecode(aws_iam_role_policy.workload["cert_manager"].policy).Statement[0].Condition.StringLike["acm-pca:TemplateArn"] == "arn:aws:acm-pca:::template/EndEntity*"
    error_message = "cert-manager's role cannot request a CA certificate"
  }
}

run "a_database_too_small_for_provisioned_iops_is_refused" {
  command = plan
  variables {
    database_storage_gb = 100
  }
  expect_failures = [var.database_storage_gb]
}

run "secrets_and_objects_are_encrypted_with_the_data_key" {
  command = apply

  assert {
    condition     = alltrue([for s in aws_secretsmanager_secret.cell : s.kms_key_id == aws_kms_key.data.arn && startswith(s.name, "erp/dev-1/")])
    error_message = "the cell's secrets are under erp/<cell>/ and encrypted with its data key"
  }
  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.objects.rule).apply_server_side_encryption_by_default).sse_algorithm == "aws:kms"
    error_message = "objects are encrypted with KMS"
  }
}

run "a_second_cell_in_the_region_leaves_the_account_defaults_alone" {
  command = apply
  variables {
    manage_ebs_defaults = false
  }
  assert {
    condition     = length(aws_ebs_encryption_by_default.cell) == 0 && length(aws_ebs_default_kms_key.cell) == 0
    error_message = "only one cell manages the account's EBS defaults"
  }
}

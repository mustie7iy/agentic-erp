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
      arn      = "arn:aws:eks:eu-central-1:222222222222:cluster/erp-dev1"
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
  mock_data "aws_servicequotas_service_quota" {
    defaults = { quota_code = "L-0000TEST", value = 100 }
  }
  mock_resource "aws_backup_vault" {
    defaults = { arn = "arn:aws:backup:eu-west-1:222222222222:backup-vault:erp-dev1-dr" }
  }
  mock_resource "aws_rds_cluster" {
    defaults = { arn = "arn:aws:rds:eu-central-1:222222222222:cluster:erp-dev1" }
  }
  mock_resource "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::erp-dev1-objects-x" }
  }
  mock_resource "aws_launch_template" {
    defaults = { id = "lt-0123456789abcdef0", latest_version = 1 }
  }
  mock_resource "aws_db_instance" {
    defaults = {
      address            = "erp-dev1.abc.eu-central-1.rds.amazonaws.com"
      master_user_secret = [{ secret_arn = "arn:aws:secretsmanager:eu-central-1:222222222222:secret:rds!db-1", kms_key_id = "k", secret_status = "active" }]
    }
  }
  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:eu-central-1:222222222222:log-group:/erp/dev1/containers" }
  }
  mock_resource "aws_secretsmanager_secret" {
    defaults = { arn = "arn:aws:secretsmanager:eu-central-1:222222222222:secret:erp/dev1/x" }
  }
}

variables {
  name                      = "dev1"
  cidr                      = "10.10.0.0/16"
  availability_zones        = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
  admin_role_arns           = ["arn:aws:iam::222222222222:role/platform-admin"]
  subordinate_ca_arn        = "arn:aws:acm-pca:eu-central-1:222222222222:certificate-authority/1111"
  image_registry_account_id = "111111111111"
  autoscaling_role_arn      = "arn:aws:iam::222222222222:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling"
}

run "the_database_requires_tls_1_3_and_survives_a_zone" {
  command = apply

  assert {
    condition     = { for p in aws_rds_cluster_parameter_group.cell[0].parameter : p.name => p.value }["ssl_min_protocol_version"] == "TLSv1.3"
    error_message = "PostgreSQL accepts TLS 1.3 and nothing older (T38)"
  }
  assert {
    condition     = { for p in aws_rds_cluster_parameter_group.cell[0].parameter : p.name => p.value }["rds.force_ssl"] == "1"
    error_message = "PostgreSQL refuses clients without TLS"
  }
  assert {
    condition     = aws_rds_cluster.cell[0].db_cluster_instance_class != null && aws_rds_cluster.cell[0].engine == "postgres" && aws_rds_cluster.cell[0].engine_version == "17.6"
    error_message = "a Multi-AZ DB cluster of PostgreSQL 17"
  }
  assert {
    condition     = aws_rds_cluster.cell[0].storage_encrypted && aws_rds_cluster.cell[0].deletion_protection && aws_rds_cluster.cell[0].backup_retention_period == 14
    error_message = "encrypted, protected from deletion, two weeks of point-in-time recovery"
  }
  assert {
    condition     = aws_rds_cluster.cell[0].manage_master_user_password
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
    condition     = jsondecode(aws_iam_role.workload["analytics"].assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.eu-central-1.amazonaws.com/id/ABC:sub"] == "system:serviceaccount:erp:erp-analytics"
    error_message = "the analytics role trusts the erp/erp-analytics service account only"
  }
  assert {
    condition     = toset(flatten([for st in jsondecode(aws_iam_role_policy.workload["analytics"].policy).Statement : st.Action])) == toset(["s3:GetObject", "s3:ListBucket", "kms:Decrypt"])
    error_message = "the analytics role reads and writes nothing else"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.workload["analytics"].policy).Statement : st.Resource if st.Sid == "ReadSnapshots"][0] == "${aws_s3_bucket.objects.arn}/analytics/*"
    error_message = "the analytics role reads analytics/ alone"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.workload["analytics"].policy).Statement : st.Condition.StringLike["s3:prefix"] if st.Sid == "ListSnapshots"][0] == "analytics/*"
    error_message = "the analytics role lists analytics/ alone"
  }

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

run "the_controllers_may_do_what_the_charts_need_only" {
  command = apply

  assert {
    condition     = toset(flatten([for st in jsondecode(aws_iam_role_policy.workload["load_balancer"].policy).Statement : st.Action if st.Sid == "Targets"])) == toset(["elasticloadbalancing:RegisterTargets", "elasticloadbalancing:DeregisterTargets"])
    error_message = "the load balancer controller registers targets and changes nothing else"
  }
  assert {
    condition     = alltrue([for st in jsondecode(aws_iam_role_policy.workload["load_balancer"].policy).Statement : alltrue([for a in flatten([st.Action]) : can(regex("^(elasticloadbalancing:(Describe\\*|RegisterTargets|DeregisterTargets)|ec2:Describe[A-Za-z]+)$", a))])])
    error_message = "no load balancer, listener or security group change"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.workload["kyverno"].policy).Statement : st.Resource if st.Sid == "ReadImages"][0] == "arn:aws:ecr:*:111111111111:repository/erp*"
    error_message = "Kyverno reads the release repositories in tooling, in any region"
  }
  assert {
    condition     = toset(flatten([for st in jsondecode(aws_iam_role_policy.workload["kyverno"].policy).Statement : st.Action if st.Sid == "ReadImages"])) == toset(["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability", "ecr:DescribeImages", "ecr:ListImages"])
    error_message = "the same read actions as the repositories grant the environments (modules/ecr pull_actions)"
  }
  assert {
    condition     = jsondecode(aws_iam_role.workload["kyverno"].assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.eu-central-1.amazonaws.com/id/ABC:sub"] == "system:serviceaccount:kyverno:kyverno-admission-controller"
    error_message = "the admission controller's service account only"
  }
  assert {
    condition     = aws_eks_addon.ebs_csi.service_account_role_arn == aws_iam_role.workload["ebs_csi"].arn
    error_message = "the EBS CSI add-on runs as its role"
  }
  assert {
    condition     = jsondecode(aws_eks_addon.vpc_cni.configuration_values).enableNetworkPolicy == "true"
    error_message = "the VPC CNI enforces network policies"
  }
}

run "logs_and_traces_leave_through_roles_of_their_own" {
  command = apply

  assert {
    condition     = aws_cloudwatch_log_group.containers.name == "/erp/dev1/containers" && aws_cloudwatch_log_group.containers.retention_in_days == 90 && aws_cloudwatch_log_group.containers.kms_key_id == aws_kms_key.cluster.arn
    error_message = "the containers' logs are kept 90 days, encrypted with the cluster key"
  }
  assert {
    condition     = jsondecode(aws_iam_role_policy.workload["logging"].policy).Statement == [{ Sid = "ContainerLogs", Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents"], Resource = ["arn:aws:logs:eu-central-1:222222222222:log-group:/erp/dev1/containers", "arn:aws:logs:eu-central-1:222222222222:log-group:/erp/dev1/containers:log-stream:*"] }]
    error_message = "Fluent Bit writes streams of its log group and nothing else: no group, retention or key change"
  }
  assert {
    condition     = jsondecode(aws_iam_role.workload["logging"].assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.eu-central-1.amazonaws.com/id/ABC:sub"] == "system:serviceaccount:logging:erp-logging"
    error_message = "the logging role trusts Fluent Bit's service account alone"
  }
  assert {
    condition     = jsondecode(aws_iam_role_policy.workload["tracing"].policy).Statement == [{ Sid = "Traces", Effect = "Allow", Action = ["xray:PutTraceSegments", "xray:PutTelemetryRecords"], Resource = "*" }]
    error_message = "the collector writes traces to X-Ray and does nothing else"
  }
  assert {
    condition     = jsondecode(aws_iam_role.workload["tracing"].assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.eu-central-1.amazonaws.com/id/ABC:sub"] == "system:serviceaccount:tracing:erp-tracing"
    error_message = "the tracing role trusts the collector's service account alone"
  }
  assert {
    condition     = output.container_log_group == "/erp/dev1/containers"
    error_message = "the log group is an output, for the fluent-bit add-on's values"
  }
}

run "only_the_server_unwraps_tenant_keys" {
  command = apply

  assert {
    condition = alltrue([
      for st in jsondecode(aws_kms_key.fields.policy).Statement :
      # Wildcards too: kms:Create* would include CreateGrant.
      !anytrue([for a in flatten([st.Action]) : can(regex("^kms:(\\*|Create\\*|CreateGrant|Decrypt|Encrypt|ReEncrypt.*|GenerateDataKey.*)$", a))])
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

run "dev_and_test_run_a_single_instance_that_still_requires_tls_1_3" {
  command = apply
  # Its own state: the production shape of the other runs is kept from destruction.
  state_key = "small"
  variables {
    zone_redundant = false
  }

  assert {
    condition     = length(aws_rds_cluster.cell) == 0 && length(aws_db_instance.cell) == 1 && !aws_db_instance.cell[0].multi_az
    error_message = "a single-AZ instance instead of the Multi-AZ DB cluster"
  }
  assert {
    condition     = aws_db_instance.cell[0].instance_class == "db.t4g.medium" && aws_db_instance.cell[0].allocated_storage == 20 && aws_db_instance.cell[0].storage_type == "gp3"
    error_message = "small gp3 storage on a small instance"
  }
  assert {
    condition     = { for p in aws_db_parameter_group.cell[0].parameter : p.name => p.value }["ssl_min_protocol_version"] == "TLSv1.3" && { for p in aws_db_parameter_group.cell[0].parameter : p.name => p.value }["rds.force_ssl"] == "1"
    error_message = "TLS 1.3 only, as in production"
  }
  assert {
    condition     = aws_db_instance.cell[0].storage_encrypted && aws_db_instance.cell[0].kms_key_id == aws_kms_key.data.arn && aws_db_instance.cell[0].deletion_protection && aws_db_instance.cell[0].backup_retention_period >= 14
    error_message = "encrypted with the data key, protected, two weeks of recovery"
  }
  assert {
    condition     = aws_db_instance.cell[0].availability_zone == "eu-central-1a"
    error_message = "in the zone of the NAT gateway and the endpoints"
  }
  assert {
    condition     = output.database_endpoint == "erp-dev1.abc.eu-central-1.rds.amazonaws.com" && output.database_master_secret_arn == "arn:aws:secretsmanager:eu-central-1:222222222222:secret:rds!db-1"
    error_message = "the outputs name the single instance's host and master secret"
  }
  assert {
    condition     = contains(jsondecode(aws_iam_role_policy.workload["external_secrets"].policy).Statement[0].Resource, "arn:aws:secretsmanager:eu-central-1:222222222222:secret:rds!db-1")
    error_message = "External Secrets reads the master credentials into the database URLs"
  }
}

run "a_small_instance_may_have_small_storage" {
  command = plan
  # Its own state: the production shape of the other runs is kept from destruction.
  state_key = "small"
  variables {
    zone_redundant      = false
    database_storage_gb = 50
  }
  assert {
    condition     = aws_db_instance.cell[0].allocated_storage == 50
    error_message = "the 400 GiB floor is the Multi-AZ DB cluster's only"
  }
}

run "a_single_instance_below_gp3_minimum_is_refused" {
  command   = plan
  state_key = "small"
  variables {
    zone_redundant      = false
    database_storage_gb = 10
  }
  expect_failures = [var.database_storage_gb]
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
    condition     = alltrue([for s in aws_secretsmanager_secret.cell : s.kms_key_id == aws_kms_key.data.arn && startswith(s.name, "erp/dev1/")])
    error_message = "the cell's secrets are under erp/<cell>/ and encrypted with its data key"
  }
  assert {
    condition     = contains(keys(aws_secretsmanager_secret.cell), "argocd-repository")
    error_message = "the cell's Argo CD has a secret for its deploy key"
  }
  assert {
    # The mock gives every secret one ARN: the statement names all but
    # the deploy key and the monitoring add-on's two.
    condition     = length([for r in jsondecode(aws_iam_role_policy.workload["external_secrets"].policy).Statement[0].Resource : r if r == aws_secretsmanager_secret.cell["argocd-repository"].arn]) == length(aws_secretsmanager_secret.cell) - 3
    error_message = "the External Secrets Operator cannot read the deploy key, nor the monitoring add-on's secrets"
  }
  assert {
    condition     = length(jsondecode(aws_iam_role_policy.workload["monitoring"].policy).Statement[0].Resource) == 2 && contains(keys(aws_secretsmanager_secret.cell), "alertmanager-receivers") && contains(keys(aws_secretsmanager_secret.cell), "grafana-oidc")
    error_message = "the monitoring add-on's role reads its two secrets and nothing else"
  }
  assert {
    condition     = jsondecode(aws_iam_role.workload["monitoring"].assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.eu-central-1.amazonaws.com/id/ABC:sub"] == "system:serviceaccount:monitoring:erp-monitoring-secrets"
    error_message = "the monitoring add-on's role trusts its SecretStore's service account alone"
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

run "a_cell_name_the_platform_refuses_is_refused" {
  command = plan
  variables {
    name = "dev-1"
  }
  expect_failures = [var.name]
}

run "the_home_cell_reads_the_global_tiers_secrets_too" {
  command = apply

  variables {
    global_secrets = {
      arns        = ["arn:aws:secretsmanager:eu-central-1:222222222222:secret:erp/global/router-public-key"]
      kms_key_arn = "arn:aws:kms:eu-central-1:222222222222:key/global"
    }
  }

  assert {
    condition     = contains(jsondecode(aws_iam_role_policy.workload["external_secrets"].policy).Statement[0].Resource, "arn:aws:secretsmanager:eu-central-1:222222222222:secret:erp/global/router-public-key")
    error_message = "the home cell's External Secrets Operator reads the global secrets it is given"
  }
  assert {
    condition     = contains(jsondecode(aws_iam_role_policy.workload["external_secrets"].policy).Statement[1].Resource, "arn:aws:kms:eu-central-1:222222222222:key/global")
    error_message = "and decrypts them with the global tier's key"
  }
  assert {
    condition     = jsondecode(aws_iam_role_policy.workload["external_secrets"].policy).Statement[1].Condition.StringEquals["kms:ViaService"] == "secretsmanager.eu-central-1.amazonaws.com"
    error_message = "through Secrets Manager only: the global key also encrypts the directory's storage"
  }
}

run "another_cell_reads_its_own_secrets_alone" {
  command = apply

  assert {
    condition     = length(jsondecode(aws_iam_role_policy.workload["external_secrets"].policy).Statement[1].Resource) == 1
    error_message = "a cell given no global secrets decrypts with its data key alone"
  }
}

run "a_cell_without_a_dr_region_keeps_two_weeks_and_its_versions_35_days" {
  command = apply

  assert {
    condition     = aws_rds_cluster.cell[0].backup_retention_period == 14
    error_message = "14 days of point-in-time recovery outside production (ADR-0040)"
  }
  assert {
    condition     = aws_s3_bucket_lifecycle_configuration.objects.rule[0].noncurrent_version_expiration[0].noncurrent_days == 35 && aws_s3_bucket_lifecycle_configuration.objects.rule[0].status == "Enabled"
    error_message = "the bucket keeps noncurrent versions as long as the longest point-in-time recovery"
  }
  assert {
    condition     = aws_s3_bucket_object_lock_configuration.objects.object_lock_enabled == "Enabled" && length(aws_s3_bucket_object_lock_configuration.objects.rule) == 0
    error_message = "object lock enabled for compliance-mode retention, with no default retention"
  }
  assert {
    condition     = length(aws_kms_key.dr) == 0 && length(aws_backup_plan.cell) == 0 && length(aws_s3_bucket.replica) == 0 && length(aws_s3_bucket_replication_configuration.objects) == 0
    error_message = "a cell without a DR region creates nothing there"
  }
}

run "a_production_cell_copies_its_backups_and_objects_to_the_dr_region" {
  command = apply
  # Its own state: the runs after it plan a cell without a DR region.
  state_key = "dr"
  variables {
    backup_retention_days = 35
    dr_region             = "eu-west-1"
  }

  assert {
    condition     = aws_rds_cluster.cell[0].backup_retention_period == 35
    error_message = "35 days of point-in-time recovery in production (ADR-0040)"
  }
  assert {
    condition     = aws_kms_key.dr["dr"].region == "eu-west-1" && aws_kms_key.dr["dr"].enable_key_rotation
    error_message = "a rotating key of the DR region"
  }
  assert {
    condition     = aws_backup_vault.dr["dr"].region == "eu-west-1" && aws_backup_vault.dr["dr"].kms_key_arn == aws_kms_key.dr["dr"].arn
    error_message = "the copies are in the DR region, encrypted with its key"
  }
  assert {
    condition     = one(aws_backup_plan.cell["dr"].rule).schedule == "cron(0 * * * ? *)"
    error_message = "the database is snapshotted every hour"
  }
  assert {
    condition     = one(one(aws_backup_plan.cell["dr"].rule).copy_action).destination_vault_arn == aws_backup_vault.dr["dr"].arn && one(one(one(aws_backup_plan.cell["dr"].rule).copy_action).lifecycle).delete_after == 7
    error_message = "every snapshot is copied to the DR vault and kept there 7 days"
  }
  assert {
    condition     = aws_servicequotas_service_quota.cluster_snapshots["dr"].region == "eu-west-1" && aws_servicequotas_service_quota.cluster_snapshots["dr"].value >= 24 * 7 && aws_servicequotas_service_quota.cluster_snapshots["home"].value >= 24
    error_message = "the snapshot quota holds a week of hourly copies in the DR region (the default, 100, does not)"
  }
  assert {
    condition     = length(setintersection(toset(jsondecode(aws_iam_role_policy.workload["server"].policy).Statement[0].Action), toset(["s3:PutObjectRetention", "s3:GetObjectRetention"]))) == 2
    error_message = "the server sets the compliance retention of audit anchors and exports (ADR-0016, ADR-0021)"
  }
  assert {
    condition     = aws_backup_selection.database["dr"].iam_role_arn == aws_iam_role.backup["dr"].arn
    error_message = "the plan runs as the backup role"
  }
  assert {
    condition     = tolist(aws_backup_selection.database["dr"].resources) == tolist([aws_rds_cluster.cell[0].arn])
    error_message = "the plan backs up the cell's database cluster"
  }
  assert {
    condition     = aws_s3_bucket.replica["dr"].region == "eu-west-1" && aws_s3_bucket.replica["dr"].object_lock_enabled
    error_message = "the replica bucket is in the DR region with object lock, which replicating locked objects needs"
  }
  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.replica["dr"].rule).apply_server_side_encryption_by_default).kms_master_key_id == aws_kms_key.dr["dr"].arn
    error_message = "the replica is encrypted with the DR region's key"
  }
  assert {
    condition     = aws_s3_bucket_lifecycle_configuration.replica["dr"].rule[0].noncurrent_version_expiration[0].noncurrent_days == 35
    error_message = "the replica keeps noncurrent versions as long as the bucket"
  }
  assert {
    condition     = one(aws_s3_bucket_replication_configuration.objects["dr"].rule).delete_marker_replication[0].status == "Enabled"
    error_message = "deletions replicate as delete markers, so they are noncurrent versions in both"
  }
  assert {
    condition     = one(one(one(aws_s3_bucket_replication_configuration.objects["dr"].rule).destination).encryption_configuration).replica_kms_key_id == aws_kms_key.dr["dr"].arn && one(aws_s3_bucket_replication_configuration.objects["dr"].rule).destination[0].bucket == aws_s3_bucket.replica["dr"].arn
    error_message = "objects replicate to the replica, encrypted with the DR region's key"
  }
}

run "more_than_35_days_of_recovery_is_refused" {
  command = plan
  variables {
    backup_retention_days = 36
  }
  expect_failures = [var.backup_retention_days]
}

run "a_dr_region_for_the_single_instance_shape_is_refused" {
  command   = plan
  state_key = "single"
  variables {
    zone_redundant = false
    dr_region      = "eu-west-1"
  }
  expect_failures = [var.dr_region]
}

run "a_production_cell_keeps_a_pilot_light_in_the_dr_region" {
  command   = apply
  state_key = "pilot"
  variables {
    backup_retention_days     = 35
    dr_region                 = "eu-west-1"
    pilot_light               = true
    dr_cidr                   = "10.65.0.0/16"
    dr_availability_zones     = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]
    replica_lag_alarm_actions = ["arn:aws:sns:eu-west-1:222222222222:erp-oncall"]
  }

  assert {
    condition     = aws_db_instance.dr["dr"].region == "eu-west-1" && aws_db_instance.dr["dr"].replicate_source_db == aws_rds_cluster.cell[0].arn
    error_message = "a cross-region read replica of the cell's database"
  }
  assert {
    condition     = aws_db_instance.dr["dr"].storage_encrypted && aws_db_instance.dr["dr"].kms_key_id == aws_kms_key.dr["dr"].arn && aws_db_instance.dr["dr"].deletion_protection
    error_message = "the replica is encrypted with the DR key and protected from deletion"
  }
  assert {
    condition     = { for p in aws_db_parameter_group.dr["dr"].parameter : p.name => p.value }["ssl_min_protocol_version"] == "TLSv1.3"
    error_message = "the replica requires TLS 1.3 as the primary does"
  }
  assert {
    condition     = aws_cloudwatch_metric_alarm.replica_lag["dr"].alarm_actions == toset(["arn:aws:sns:eu-west-1:222222222222:erp-oncall"]) && aws_cloudwatch_metric_alarm.replica_lag["dr"].metric_name == "ReplicaLag" && aws_cloudwatch_metric_alarm.replica_lag["dr"].threshold == 60 && aws_cloudwatch_metric_alarm.replica_lag["dr"].region == "eu-west-1"
    error_message = "an alarm when the replica is more than the RPO (60 s) behind"
  }
  assert {
    condition     = aws_eks_cluster.dr["dr"].region == "eu-west-1" && one(one(aws_eks_cluster.dr["dr"].encryption_config).provider).key_arn == aws_kms_key.dr_cluster["dr"].arn && !aws_eks_cluster.dr["dr"].vpc_config[0].endpoint_public_access
    error_message = "a private EKS control plane in the DR region, its secrets encrypted with the DR cluster key"
  }
  assert {
    condition     = length(aws_eks_access_entry.dr_admin) == 1
    error_message = "the administrators reach the DR cluster"
  }
  assert {
    condition     = aws_kms_key.fields.multi_region && aws_kms_replica_key.fields["dr"].primary_key_arn == aws_kms_key.fields.arn && aws_kms_replica_key.fields["dr"].region == "eu-west-1"
    error_message = "the field key is replicated, so tenant keys wrapped at home unwrap in the DR region"
  }
  assert {
    condition     = alltrue([for s in aws_secretsmanager_secret.cell : one(s.replica).region == "eu-west-1"])
    error_message = "the secrets are replicated to the DR region"
  }
}

# The pilot light runs the cell on a failover (T41b3, T72j): nodes at
# zero, the workloads' roles trusting the DR cluster alone and doing there,
# on the DR region's counterparts, what they do at home. The DR resources
# get values of their own, so the checks tell the two regions apart.
run "the_pilot_light_can_run_the_cell_in_the_dr_region" {
  command   = apply
  state_key = "pilot_runs"
  variables {
    backup_retention_days     = 35
    dr_region                 = "eu-west-1"
    pilot_light               = true
    dr_cidr                   = "10.65.0.0/16"
    dr_availability_zones     = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]
    replica_lag_alarm_actions = ["arn:aws:sns:eu-west-1:222222222222:erp-oncall"]
  }
  override_resource {
    target = aws_eks_cluster.dr
    values = {
      arn      = "arn:aws:eks:eu-west-1:222222222222:cluster/erp-dev1"
      endpoint = "https://DR.gr7.eu-west-1.eks.amazonaws.com"
      identity = [{ oidc = [{ issuer = "https://oidc.eks.eu-west-1.amazonaws.com/id/DR" }] }]
    }
  }
  override_resource {
    target = aws_iam_openid_connect_provider.dr
    values = { arn = "arn:aws:iam::222222222222:oidc-provider/oidc.eks.eu-west-1.amazonaws.com/id/DR" }
  }
  override_resource {
    target = aws_iam_role.dr_workload
    values = { arn = "arn:aws:iam::222222222222:role/dr-mock" }
  }
  override_resource {
    target = aws_kms_key.dr
    values = { arn = "arn:aws:kms:eu-west-1:222222222222:key/dr-data" }
  }
  override_resource {
    target = aws_kms_key.dr_cluster
    values = { arn = "arn:aws:kms:eu-west-1:222222222222:key/dr-cluster" }
  }
  override_resource {
    target = aws_kms_replica_key.fields
    values = { arn = "arn:aws:kms:eu-west-1:222222222222:key/mrk-fields" }
  }
  override_resource {
    target = aws_s3_bucket.replica
    values = { arn = "arn:aws:s3:::erp-dev1-replica-x", bucket = "erp-dev1-replica-x" }
  }
  override_resource {
    target = aws_db_instance.dr
    values = { arn = "arn:aws:rds:eu-west-1:222222222222:db:erp-dev1-dr", address = "erp-dev1-dr.abc.eu-west-1.rds.amazonaws.com" }
  }
  override_resource {
    target = aws_cloudwatch_log_group.dr_containers
    values = { arn = "arn:aws:logs:eu-west-1:222222222222:log-group:/erp/dev1/containers" }
  }
  override_resource {
    target = aws_launch_template.dr_nodes
    values = { id = "lt-0dr", latest_version = 1 }
  }

  # The nodes.
  assert {
    condition     = aws_eks_node_group.dr["dr"].region == "eu-west-1" && aws_eks_node_group.dr["dr"].cluster_name == aws_eks_cluster.dr["dr"].name && aws_eks_node_group.dr["dr"].subnet_ids == toset(module.dr_network["dr"].private_subnet_ids)
    error_message = "the node group is the DR cluster's, in the DR VPC's private subnets"
  }
  assert {
    condition     = aws_eks_node_group.dr["dr"].scaling_config[0].min_size == 0 && aws_eks_node_group.dr["dr"].scaling_config[0].desired_size == 0
    error_message = "the pilot light's nodes are at zero until a failover"
  }
  assert {
    condition     = aws_eks_node_group.dr["dr"].scaling_config[0].max_size == 12 && output.pilot_light.node_scaling == { min = 3, desired = 3, max = 12 }
    error_message = "the group may grow to the cell's size, which the runbook reads from the output"
  }
  assert {
    condition     = aws_eks_node_group.dr["dr"].node_role_arn == aws_iam_role.nodes.arn && aws_eks_node_group.dr["dr"].instance_types == aws_eks_node_group.cell.instance_types && aws_eks_node_group.dr["dr"].ami_type == aws_eks_node_group.cell.ami_type
    error_message = "the DR nodes are the home nodes' role, instance types and image"
  }
  assert {
    condition     = aws_eks_node_group.dr["dr"].launch_template[0].id == "lt-0dr" && aws_launch_template.dr_nodes["dr"].region == "eu-west-1"
    error_message = "the DR nodes launch from the DR launch template"
  }
  assert {
    condition     = aws_launch_template.dr_nodes["dr"].block_device_mappings[0].ebs[0].encrypted == "true" && aws_launch_template.dr_nodes["dr"].block_device_mappings[0].ebs[0].kms_key_id == "arn:aws:kms:eu-west-1:222222222222:key/dr-cluster"
    error_message = "the DR nodes' volumes are encrypted with the DR cluster key"
  }
  assert {
    condition     = aws_launch_template.dr_nodes["dr"].metadata_options[0].http_tokens == "required" && aws_launch_template.dr_nodes["dr"].metadata_options[0].http_put_response_hop_limit == 1
    error_message = "pods cannot reach the DR nodes' instance metadata; they use IRSA"
  }
  assert {
    condition     = [for st in jsondecode(aws_kms_key.dr_cluster["dr"].policy).Statement : st.Principal.AWS if st.Sid == "AutoScalingVolumes"] == [var.autoscaling_role_arn] && [for st in jsondecode(aws_kms_key.dr_cluster["dr"].policy).Statement : st.Condition if st.Sid == "AutoScalingVolumeGrants"][0].Bool["kms:GrantIsForAWSResource"] == "true"
    error_message = "Auto Scaling may launch the DR nodes' encrypted volumes, and grant for AWS resources only"
  }
  assert {
    condition     = aws_eks_addon.dr_vpc_cni["dr"].region == "eu-west-1" && aws_eks_addon.dr_vpc_cni["dr"].cluster_name == aws_eks_cluster.dr["dr"].name && jsondecode(aws_eks_addon.dr_vpc_cni["dr"].configuration_values).enableNetworkPolicy == "true"
    error_message = "the DR cluster's VPC CNI enforces network policies"
  }

  # The roles: the DR cluster's provider, and only it.
  assert {
    condition     = aws_iam_openid_connect_provider.dr["dr"].url == "https://oidc.eks.eu-west-1.amazonaws.com/id/DR" && aws_iam_openid_connect_provider.dr["dr"].client_id_list == toset(["sts.amazonaws.com"])
    error_message = "the DR cluster's issuer is a provider of its own, for STS"
  }
  assert {
    condition     = keys(aws_iam_role.dr_workload) == keys(aws_iam_role.workload) && keys(aws_iam_role_policy.dr_workload) == keys(aws_iam_role.workload)
    error_message = "every service account with a role at home has one, and its policy, in the DR region"
  }
  assert {
    condition = alltrue([for name, role in aws_iam_role.dr_workload :
      jsondecode(role.assume_role_policy).Statement[0].Principal.Federated == aws_iam_openid_connect_provider.dr["dr"].arn &&
      jsondecode(role.assume_role_policy).Statement[0].Condition.StringEquals == {
        "oidc.eks.eu-west-1.amazonaws.com/id/DR:aud" = "sts.amazonaws.com"
        "oidc.eks.eu-west-1.amazonaws.com/id/DR:sub" = jsondecode(aws_iam_role.workload[name].assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.eu-central-1.amazonaws.com/id/ABC:sub"]
      }
    ])
    error_message = "each DR role trusts the DR cluster's provider for its home counterpart's service account alone"
  }
  assert {
    condition     = alltrue([for role in aws_iam_role.dr_workload : length(jsondecode(role.assume_role_policy).Statement) == 1 && !strcontains(role.assume_role_policy, "eu-central-1") && !strcontains(role.assume_role_policy, "id/ABC")])
    error_message = "no DR role trusts the home cluster's OIDC provider"
  }
  assert {
    condition     = alltrue([for role in aws_iam_role.workload : !strcontains(role.assume_role_policy, "id/DR")])
    error_message = "no home role trusts the DR cluster's OIDC provider"
  }
  assert {
    condition     = alltrue([for name, policy in aws_iam_role_policy.dr_workload : toset(flatten([for st in jsondecode(policy.policy).Statement : st.Action])) == toset(flatten([for st in jsondecode(aws_iam_role_policy.workload[name].policy).Statement : st.Action]))])
    error_message = "each DR role may do what its home counterpart does, no more"
  }
  assert {
    condition     = alltrue([for name, policy in aws_iam_role_policy.dr_workload : !strcontains(policy.policy, "eu-central-1") && !strcontains(policy.policy, "objects-x") && !strcontains(policy.policy, "key/0000") if name != "cert_manager"])
    error_message = "the DR roles use the DR region's bucket, keys, secrets and logs, none of the home region's"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.dr_workload["cert_manager"].policy).Statement : st.Resource] == [var.subordinate_ca_arn, var.subordinate_ca_arn]
    error_message = "the DR issuer issues from the environment's subordinate CA, the only one it has"
  }
  assert {
    condition     = aws_iam_role_policy_attachment.dr_ebs_csi["dr"].role == aws_iam_role.dr_workload["ebs_csi"].name
    error_message = "the DR EBS CSI driver's role may operate volumes"
  }

  # What each DR role reaches.
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.dr_workload["server"].policy).Statement : st.Resource] == ["arn:aws:s3:::erp-dev1-replica-x/*", "arn:aws:s3:::erp-dev1-replica-x", "arn:aws:kms:eu-west-1:222222222222:key/dr-data"]
    error_message = "the DR server uses the bucket's replica under the DR key"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.dr_workload["analytics"].policy).Statement : st.Resource if st.Sid == "ReadSnapshots"] == ["arn:aws:s3:::erp-dev1-replica-x/analytics/*"] && [for st in jsondecode(aws_iam_role_policy.dr_workload["analytics"].policy).Statement : st.Condition if st.Sid == "SnapshotKey"][0].StringEquals["kms:ViaService"] == "s3.eu-west-1.amazonaws.com"
    error_message = "DR analytics reads the replica's analytics/ alone, the key through S3 of the DR region"
  }
  assert {
    condition     = alltrue([for arn in [for st in jsondecode(aws_iam_role_policy.dr_workload["external_secrets"].policy).Statement : st.Resource if st.Sid == "Secrets"][0] : startswith(arn, "arn:aws:secretsmanager:eu-west-1:222222222222:secret:erp/dev1/")]) && length([for st in jsondecode(aws_iam_role_policy.dr_workload["external_secrets"].policy).Statement : st.Resource if st.Sid == "Secrets"][0]) == length(var.secrets) - 3
    error_message = "DR External Secrets reads the secrets' replicas, not the deploy key or the monitoring add-on's"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.dr_workload["external_secrets"].policy).Statement : st.Condition if st.Sid == "MasterSecret"][0].StringEquals["secretsmanager:ResourceTag/aws:rds:primaryDBInstanceArn"] == "arn:aws:rds:eu-west-1:222222222222:db:erp-dev1-dr"
    error_message = "the master secret RDS makes for the promoted replica, and no other database's"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.dr_workload["external_secrets"].policy).Statement : st.Condition if st.Sid == "SecretsKey"][0].StringEquals["kms:ViaService"] == "secretsmanager.eu-west-1.amazonaws.com"
    error_message = "the DR key is decrypted through Secrets Manager of the DR region only"
  }
  assert {
    condition     = length([for st in jsondecode(aws_iam_role_policy.dr_workload["monitoring"].policy).Statement : st.Resource if st.Sid == "Secrets"][0]) == 2
    error_message = "the DR monitoring role reads its two secrets' replicas"
  }
  assert {
    condition     = jsondecode(aws_iam_role_policy.dr_workload["logging"].policy).Statement[0].Resource == ["arn:aws:logs:eu-west-1:222222222222:log-group:/erp/dev1/containers", "arn:aws:logs:eu-west-1:222222222222:log-group:/erp/dev1/containers:log-stream:*"]
    error_message = "DR Fluent Bit writes streams of the DR log group alone"
  }
  assert {
    condition     = aws_cloudwatch_log_group.dr_containers["dr"].region == "eu-west-1" && aws_cloudwatch_log_group.dr_containers["dr"].name == "/erp/dev1/containers" && aws_cloudwatch_log_group.dr_containers["dr"].retention_in_days == 90 && aws_cloudwatch_log_group.dr_containers["dr"].kms_key_id == "arn:aws:kms:eu-west-1:222222222222:key/dr-cluster"
    error_message = "the DR containers' log group is kept 90 days under the DR cluster key"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.dr_workload["ebs_csi"].policy).Statement : st.Resource] == ["arn:aws:kms:eu-west-1:222222222222:key/dr-cluster"]
    error_message = "the DR EBS CSI driver uses the DR cluster key"
  }
  assert {
    condition     = [for st in jsondecode(aws_iam_role_policy.dr_workload["load_balancer"].policy).Statement : st.Resource if st.Sid == "Targets"] == ["arn:aws:elasticloadbalancing:eu-west-1:222222222222:targetgroup/erp-*/*"]
    error_message = "the DR load balancer controller registers targets of the DR region's groups alone"
  }

  # The field key's replica.
  assert {
    condition     = [for st in jsondecode(aws_kms_replica_key.fields["dr"].policy).Statement : st.Principal.AWS if st.Sid != "AdministerOnly"] == ["arn:aws:iam::222222222222:role/dr-mock", "arn:aws:iam::222222222222:role/dr-mock"]
    error_message = "the DR server's role, and no other, wraps and unwraps tenant keys with the replica"
  }
  assert {
    condition     = [for st in jsondecode(aws_kms_key.fields.policy).Statement : st.Principal.AWS if st.Sid != "AdministerOnly"] == [aws_iam_role.workload["server"].arn, aws_iam_role.workload["server"].arn]
    error_message = "the home server's role wraps with the key at home"
  }
  assert {
    condition = alltrue([
      for st in jsondecode(aws_kms_replica_key.fields["dr"].policy).Statement :
      !anytrue([for a in flatten([st.Action]) : can(regex("^kms:(\\*|Create\\*|CreateGrant|Decrypt|Encrypt|ReEncrypt.*|GenerateDataKey.*)$", a))])
      if st.Sid != "ServerWrapsTenantKeys"
    ]) && [for st in jsondecode(aws_kms_replica_key.fields["dr"].policy).Statement : st.Condition if st.Sid == "ServerWrapsTenantKeys"][0]["ForAllValues:StringEquals"]["kms:EncryptionContextKeys"] == ["tenant"]
    error_message = "the replica's policy is the key's: administrators manage it but cannot use it, the server only with a tenant context"
  }

  # The values the cell takes there.
  assert {
    condition     = output.pilot_light.region == "eu-west-1" && output.pilot_light.cluster_name == "erp-dev1" && output.pilot_light.cluster_endpoint == "https://DR.gr7.eu-west-1.eks.amazonaws.com" && output.pilot_light.node_group == "erp-dev1-general"
    error_message = "the DR cluster and its node group are outputs"
  }
  assert {
    condition     = output.pilot_light.database_instance == "erp-dev1-dr" && output.pilot_light.database_endpoint == "erp-dev1-dr.abc.eu-west-1.rds.amazonaws.com" && output.pilot_light.database_master_secret_kms_key_arn == "arn:aws:kms:eu-west-1:222222222222:key/dr-data"
    error_message = "the replica, the endpoint the cell's values take once it is promoted, and the key its master secret is made under"
  }
  assert {
    condition     = output.pilot_light.object_bucket == "erp-dev1-replica-x" && output.pilot_light.field_kms_key_arn == "arn:aws:kms:eu-west-1:222222222222:key/mrk-fields" && output.pilot_light.cluster_kms_key_arn == "arn:aws:kms:eu-west-1:222222222222:key/dr-cluster" && output.pilot_light.container_log_group == "/erp/dev1/containers"
    error_message = "the bucket's replica, the field key's replica, the DR cluster key and the DR log group are the cell's values there"
  }
  assert {
    condition     = output.pilot_light.workload_role_arns == { for name, role in aws_iam_role.dr_workload : name => role.arn } && output.pilot_light.aws_endpoints == { sts = "https://sts.eu-west-1.amazonaws.com", kms = "https://kms.eu-west-1.amazonaws.com", s3 = "https://s3.eu-west-1.amazonaws.com" }
    error_message = "the DR roles and the DR region's endpoints are the cell's values there"
  }
}

run "a_cell_without_a_pilot_light_has_none" {
  command   = plan
  state_key = "dr"
  variables {
    backup_retention_days = 35
    dr_region             = "eu-west-1"
  }

  assert {
    condition     = length(aws_db_instance.dr) == 0 && length(aws_eks_cluster.dr) == 0 && length(module.dr_network) == 0 && length(aws_ecs_cluster.drill) == 0 && output.restore_drill == null && !aws_kms_key.fields.multi_region && length(aws_kms_replica_key.fields) == 0 && alltrue([for s in aws_secretsmanager_secret.cell : length(s.replica) == 0])
    error_message = "staging has the backup copies only"
  }
  assert {
    condition     = length(aws_eks_node_group.dr) == 0 && length(aws_iam_openid_connect_provider.dr) == 0 && length(aws_iam_role.dr_workload) == 0 && length(aws_cloudwatch_log_group.dr_containers) == 0 && output.pilot_light == null
    error_message = "without a pilot light no DR nodes, provider, roles or log group"
  }
}

run "a_pilot_light_without_a_dr_region_is_refused" {
  command   = plan
  state_key = "refused"
  variables {
    pilot_light = true
  }
  expect_failures = [var.pilot_light]
}

run "a_pilot_light_without_its_network_is_refused" {
  command   = plan
  state_key = "refused"
  variables {
    pilot_light = true
    dr_region   = "eu-west-1"
  }
  expect_failures = [var.dr_cidr, var.dr_availability_zones, var.replica_lag_alarm_actions]
}

run "staging_verifies_its_copies_from_a_task_in_the_dr_region" {
  command   = apply
  state_key = "drill"
  variables {
    dr_region             = "eu-west-1"
    restore_drill_dr      = true
    dr_cidr               = "10.49.0.0/16"
    dr_availability_zones = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]
  }

  assert {
    condition     = length(module.dr_network) == 1 && aws_db_subnet_group.dr["dr"].region == "eu-west-1" && aws_security_group.dr_database["dr"].region == "eu-west-1"
    error_message = "the copies are restored into a VPC of the DR region"
  }
  assert {
    condition     = length(aws_db_instance.dr) == 0 && length(aws_eks_cluster.dr) == 0 && !aws_kms_key.fields.multi_region
    error_message = "the drill's side is no pilot light: no replica, no control plane"
  }
  assert {
    condition     = aws_ecs_cluster.drill["dr"].region == "eu-west-1" && aws_cloudwatch_log_group.drill["dr"].kms_key_id == aws_kms_key.dr["dr"].arn
    error_message = "the verification runs on a Fargate cluster of the DR region and logs encrypted with the DR key"
  }
  assert {
    condition     = contains([for s in jsondecode(aws_kms_key.dr["dr"].policy).Statement : s.Sid if can(s.Sid)], "CloudWatchLogs")
    error_message = "CloudWatch Logs may use the DR key for the drill's log group"
  }
  assert {
    condition     = toset([for e in aws_security_group.drill["dr"].egress : e.from_port]) == toset([5432, 443]) && length(aws_security_group.drill["dr"].ingress) == 0
    error_message = "the task reaches the database and HTTPS only, and nothing reaches it"
  }
  assert {
    condition     = endswith(jsondecode(aws_iam_role_policy.drill["dr"].policy).Statement[0].Condition.StringLike["secretsmanager:ResourceTag/aws:rds:primaryDBClusterArn"], ":cluster:${aws_rds_cluster.cell[0].cluster_identifier}-drill-*")
    error_message = "the task reads the master secrets of the drill's side clusters only"
  }
  assert {
    condition     = output.restore_drill.cluster == aws_ecs_cluster.drill["dr"].name && output.restore_drill.db_subnet_group == aws_db_subnet_group.dr["dr"].name && output.restore_drill.vault == aws_backup_vault.dr["dr"].name
    error_message = "the drill's settings are an output for scripts/restore-drill.sh"
  }
}

run "a_drill_side_without_a_dr_region_is_refused" {
  command   = plan
  state_key = "refused"
  variables {
    restore_drill_dr = true
  }
  expect_failures = [var.restore_drill_dr]
}

run "a_drill_side_without_its_network_is_refused" {
  command   = plan
  state_key = "refused"
  variables {
    restore_drill_dr = true
    dr_region        = "eu-west-1"
  }
  expect_failures = [var.dr_cidr, var.dr_availability_zones]
}

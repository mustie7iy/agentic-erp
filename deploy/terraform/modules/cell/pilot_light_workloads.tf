# The pilot light's workload roles (ADR-0040, T41b3): one per service
# account of irsa.tf, each trusting exactly that namespace and service
# account of the DR cluster, through the DR cluster's own OIDC provider,
# and doing there what its home counterpart does at home, on the DR
# region's counterparts: the bucket's replica, the DR key (the data key's
# counterpart: the replica bucket, the secrets' replicas, the database
# replica), the DR cluster key, the secrets' replicas and the DR region's
# log group. No role trusts both clusters: a token of one cluster assumes
# nothing in the other region. The global tier's secrets are not read
# there: the router and the directory are in the home region (modules/global).

locals {
  dr_oidc_host = { for k in local.pilot : k => replace(aws_iam_openid_connect_provider.dr[k].url, "https://", "") }
  # A replica secret keeps its primary's name and ARN but for the region.
  dr_secret_arns     = { for k in local.pilot : k => { for name, secret in aws_secretsmanager_secret.cell : name => replace(secret.arn, ":${data.aws_region.current.region}:", ":${var.dr_region}:") } }
  dr_secrets_key_via = { for k in local.pilot : k => { StringEquals = { "kms:ViaService" = "secretsmanager.${var.dr_region}.amazonaws.com" } } }

  dr_workloads = { for k in local.pilot : k => {
    server = {
      statements = [
        { Sid = "Objects", Effect = "Allow", Action = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:PutObjectRetention", "s3:GetObjectRetention"], Resource = "${aws_s3_bucket.replica[k].arn}/*" },
        { Sid = "ListObjects", Effect = "Allow", Action = "s3:ListBucket", Resource = aws_s3_bucket.replica[k].arn },
        { Sid = "ObjectKey", Effect = "Allow", Action = ["kms:Decrypt", "kms:GenerateDataKey"], Resource = aws_kms_key.dr[k].arn },
      ]
    }
    analytics = {
      statements = [
        { Sid = "ReadSnapshots", Effect = "Allow", Action = "s3:GetObject", Resource = "${aws_s3_bucket.replica[k].arn}/analytics/*" },
        { Sid = "ListSnapshots", Effect = "Allow", Action = "s3:ListBucket", Resource = aws_s3_bucket.replica[k].arn, Condition = { StringLike = { "s3:prefix" = "analytics/*" } } },
        { Sid = "SnapshotKey", Effect = "Allow", Action = "kms:Decrypt", Resource = aws_kms_key.dr[k].arn, Condition = { StringEquals = { "kms:ViaService" = "s3.${var.dr_region}.amazonaws.com" } } },
      ]
    }
    # The secrets' replicas but the deploy key and the monitoring
    # add-on's, as at home. The promoted replica has no master secret
    # until the runbook has RDS manage one (in the DR region, under the DR
    # key); RDS names it rds!db-<id> and tags it with its instance, which
    # scopes this to the replica's.
    external_secrets = {
      statements = [
        { Sid = "Secrets", Effect = "Allow", Action = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"], Resource = [for name, arn in local.dr_secret_arns[k] : arn if !contains(concat(["argocd-repository"], local.monitoring_secrets), name)] },
        { Sid = "MasterSecret", Effect = "Allow", Action = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"], Resource = "arn:${data.aws_partition.current.partition}:secretsmanager:${var.dr_region}:${data.aws_caller_identity.current.account_id}:secret:rds!db-*", Condition = { StringEquals = { "secretsmanager:ResourceTag/aws:rds:primaryDBInstanceArn" = aws_db_instance.dr[k].arn } } },
        { Sid = "SecretsKey", Effect = "Allow", Action = "kms:Decrypt", Resource = aws_kms_key.dr[k].arn, Condition = local.dr_secrets_key_via[k] },
      ]
    }
    # The same subordinate CA: ACM Private CA is regional and the
    # environment has one, at home (modules/subordinate-ca).
    cert_manager = {
      statements = [
        { Sid = "Issue", Effect = "Allow", Action = "acm-pca:IssueCertificate", Resource = var.subordinate_ca_arn, Condition = { StringLike = { "acm-pca:TemplateArn" = "arn:${data.aws_partition.current.partition}:acm-pca:::template/EndEntity*" } } },
        { Sid = "Read", Effect = "Allow", Action = ["acm-pca:GetCertificate", "acm-pca:DescribeCertificateAuthority"], Resource = var.subordinate_ca_arn },
      ]
    }
    ebs_csi = {
      statements = [
        { Sid = "VolumeKey", Effect = "Allow", Action = ["kms:CreateGrant", "kms:ListGrants", "kms:RevokeGrant", "kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"], Resource = aws_kms_key.dr_cluster[k].arn },
      ]
    }
    load_balancer = {
      statements = [
        { Sid = "Read", Effect = "Allow", Action = ["elasticloadbalancing:Describe*", "ec2:DescribeInstances", "ec2:DescribeNetworkInterfaces", "ec2:DescribeSubnets", "ec2:DescribeSecurityGroups", "ec2:DescribeVpcs", "ec2:DescribeAvailabilityZones"], Resource = "*" },
        { Sid = "Targets", Effect = "Allow", Action = ["elasticloadbalancing:RegisterTargets", "elasticloadbalancing:DeregisterTargets"], Resource = "arn:${data.aws_partition.current.partition}:elasticloadbalancing:${var.dr_region}:${data.aws_caller_identity.current.account_id}:targetgroup/erp-*/*" },
      ]
    }
    # The registry's replica in the DR region is tooling's too (modules/ecr
    # replication_regions), so the same statement as at home.
    kyverno = local.workloads.kyverno
    monitoring = {
      statements = [
        { Sid = "Secrets", Effect = "Allow", Action = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"], Resource = [for name in local.monitoring_secrets : local.dr_secret_arns[k][name]] },
        { Sid = "SecretsKey", Effect = "Allow", Action = "kms:Decrypt", Resource = aws_kms_key.dr[k].arn, Condition = local.dr_secrets_key_via[k] },
      ]
    }
    logging = {
      statements = [
        { Sid = "ContainerLogs", Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents"], Resource = [aws_cloudwatch_log_group.dr_containers[k].arn, "${aws_cloudwatch_log_group.dr_containers[k].arn}:log-stream:*"] },
      ]
    }
    tracing = local.workloads.tracing
  } }
}

resource "aws_iam_openid_connect_provider" "dr" {
  for_each       = local.pilot
  url            = aws_eks_cluster.dr[each.key].identity[0].oidc[0].issuer
  client_id_list = ["sts.amazonaws.com"]
}

resource "aws_iam_role" "dr_workload" {
  for_each = { for name, account in local.service_accounts : name => account if var.pilot_light }
  name     = "${local.prefix}-dr-${replace(each.key, "_", "-")}"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.dr["dr"].arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.dr_oidc_host["dr"]}:aud" = "sts.amazonaws.com"
          "${local.dr_oidc_host["dr"]}:sub" = "system:serviceaccount:${each.value.namespace}:${each.value.name}"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "dr_workload" {
  for_each = aws_iam_role.dr_workload
  name     = "workload"
  role     = each.value.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = local.dr_workloads["dr"][each.key].statements
  })
}

resource "aws_iam_role_policy_attachment" "dr_ebs_csi" {
  for_each   = local.pilot
  role       = aws_iam_role.dr_workload["ebs_csi"].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}

# Fluent Bit's log group in the DR region (logs.tf at home).
resource "aws_cloudwatch_log_group" "dr_containers" {
  for_each          = local.pilot
  region            = var.dr_region
  name              = "/erp/${var.name}/containers"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.dr_cluster[each.key].arn
}

output "pilot_light" {
  description = "The pilot light's values in dr_region, for the cell's values on a failover (ADR-0040 runbook); null without pilot_light. The database master secret is not among them: RDS manages one for the replica only once promoted (the runbook)."
  value = var.pilot_light ? {
    region                             = var.dr_region
    cluster_name                       = aws_eks_cluster.dr["dr"].name
    cluster_endpoint                   = aws_eks_cluster.dr["dr"].endpoint
    node_group                         = aws_eks_node_group.dr["dr"].node_group_name
    node_scaling                       = var.node_scaling
    ebs_csi_addon_version              = var.ebs_csi_addon_version
    vpc_id                             = module.dr_network["dr"].vpc_id
    cidr                               = module.dr_network["dr"].cidr
    database_instance                  = aws_db_instance.dr["dr"].identifier
    database_endpoint                  = aws_db_instance.dr["dr"].address
    database_master_secret_kms_key_arn = aws_kms_key.dr["dr"].arn
    object_bucket                      = aws_s3_bucket.replica["dr"].bucket
    field_kms_key_arn                  = aws_kms_replica_key.fields["dr"].arn
    cluster_kms_key_arn                = aws_kms_key.dr_cluster["dr"].arn
    container_log_group                = aws_cloudwatch_log_group.dr_containers["dr"].name
    workload_role_arns                 = { for name, role in aws_iam_role.dr_workload : name => role.arn }
    aws_endpoints = {
      sts = "https://sts.${var.dr_region}.amazonaws.com"
      kms = "https://kms.${var.dr_region}.amazonaws.com"
      s3  = "https://s3.${var.dr_region}.amazonaws.com"
    }
  } : null
}

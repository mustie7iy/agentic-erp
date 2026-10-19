# The roles the cell's workloads assume with their service account's token
# (IRSA, ADR-0024). Each trusts exactly one namespace and service account
# of this cluster and may do only what that workload does.

locals {
  oidc_host = replace(aws_iam_openid_connect_provider.cluster.url, "https://", "")

  # Who each role trusts, apart from what it may do: the field key's policy
  # names the server's role, and the server's permissions name the key.
  service_accounts = {
    server           = { namespace = "erp", name = "erp-server" }
    external_secrets = { namespace = "external-secrets", name = "external-secrets" }
    cert_manager     = { namespace = "cert-manager", name = "aws-privateca-issuer" }
    ebs_csi          = { namespace = "kube-system", name = "ebs-csi-controller-sa" }
    load_balancer    = { namespace = "kube-system", name = "aws-load-balancer-controller" }
    kyverno          = { namespace = "kyverno", name = "kyverno-admission-controller" }
  }

  workloads = {
    # The API server and its workers: objects, field-key wrapping.
    server = {
      statements = [
        { Sid = "Objects", Effect = "Allow", Action = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:PutObjectRetention", "s3:GetObjectRetention"], Resource = "${aws_s3_bucket.objects.arn}/*" },
        { Sid = "ListObjects", Effect = "Allow", Action = "s3:ListBucket", Resource = aws_s3_bucket.objects.arn },
        { Sid = "ObjectKey", Effect = "Allow", Action = ["kms:Decrypt", "kms:GenerateDataKey"], Resource = aws_kms_key.data.arn },
      ]
    }
    # The External Secrets Operator: the cell's secrets and the database's
    # RDS-managed master credentials, read only; on the home cell also the
    # global tier's (the router's, and the directory's credentials). Not
    # the deploy key, which only the bootstrap reads, into Argo CD.
    external_secrets = {
      statements = [
        { Sid = "Secrets", Effect = "Allow", Action = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"], Resource = concat([for name, secret in aws_secretsmanager_secret.cell : secret.arn if name != "argocd-repository"], compact([local.database_master_secret_arn]), var.global_secrets.arns) },
        # Through Secrets Manager only: the global tier's key also encrypts
        # the directory's storage and logs.
        { Sid = "SecretsKey", Effect = "Allow", Action = "kms:Decrypt", Resource = compact([aws_kms_key.data.arn, var.global_secrets.kms_key_arn]), Condition = { StringEquals = { "kms:ViaService" = "secretsmanager.${data.aws_region.current.region}.amazonaws.com" } } },
      ]
    }
    # cert-manager's aws-privateca-issuer: certificates from the
    # environment's subordinate CA (T38).
    cert_manager = {
      statements = [
        # End-entity templates only: path length 0 aside, the role cannot
        # ask for a CA certificate.
        { Sid = "Issue", Effect = "Allow", Action = "acm-pca:IssueCertificate", Resource = var.subordinate_ca_arn, Condition = { StringLike = { "acm-pca:TemplateArn" = "arn:${data.aws_partition.current.partition}:acm-pca:::template/EndEntity*" } } },
        { Sid = "Read", Effect = "Allow", Action = ["acm-pca:GetCertificate", "acm-pca:DescribeCertificateAuthority"], Resource = var.subordinate_ca_arn },
      ]
    }
    # The EBS CSI driver: the volumes of NATS JetStream and the like,
    # encrypted with the cluster key.
    ebs_csi = {
      statements = [
        { Sid = "VolumeKey", Effect = "Allow", Action = ["kms:CreateGrant", "kms:ListGrants", "kms:RevokeGrant", "kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"], Resource = aws_kms_key.cluster.arn },
      ]
    }
    # The AWS Load Balancer Controller, for TargetGroupBinding only: it
    # registers the router's pods in the target group Terraform made
    # (modules/global) and creates no load balancer, listener or security
    # group rule, so it reads and registers and nothing else.
    load_balancer = {
      statements = [
        { Sid = "Read", Effect = "Allow", Action = ["elasticloadbalancing:Describe*", "ec2:DescribeInstances", "ec2:DescribeNetworkInterfaces", "ec2:DescribeSubnets", "ec2:DescribeSecurityGroups", "ec2:DescribeVpcs", "ec2:DescribeAvailabilityZones"], Resource = "*" },
        { Sid = "Targets", Effect = "Allow", Action = ["elasticloadbalancing:RegisterTargets", "elasticloadbalancing:DeregisterTargets"], Resource = "arn:${data.aws_partition.current.partition}:elasticloadbalancing:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:targetgroup/erp-*/*" },
      ]
    }
    # Kyverno's admission controller: it reads the release images'
    # signatures and attestations from the registry in tooling (any region,
    # since images are replicated) to admit them (ADR-0035).
    kyverno = {
      statements = [
        { Sid = "RegistryToken", Effect = "Allow", Action = "ecr:GetAuthorizationToken", Resource = "*" },
        # The same actions as the repositories grant the environments
        # (modules/ecr pull_actions): cross-account, both sides must allow.
        { Sid = "ReadImages", Effect = "Allow", Action = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability", "ecr:DescribeImages", "ecr:ListImages"], Resource = "arn:${data.aws_partition.current.partition}:ecr:*:${var.image_registry_account_id}:repository/erp*" },
      ]
    }
  }
}

resource "aws_iam_role" "workload" {
  for_each = local.service_accounts
  name     = "${local.prefix}-${replace(each.key, "_", "-")}"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.cluster.arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.oidc_host}:aud" = "sts.amazonaws.com"
          "${local.oidc_host}:sub" = "system:serviceaccount:${each.value.namespace}:${each.value.name}"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "workload" {
  for_each = local.workloads
  name     = "workload"
  role     = aws_iam_role.workload[each.key].id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = each.value.statements
  })
}

# The CSI driver's volume operations themselves.
resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.workload["ebs_csi"].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}

# The roles the cell's workloads assume with their service account's token
# (IRSA, ADR-0024). Each trusts exactly one namespace and service account
# of this cluster and may do only what that workload does.

locals {
  oidc_host = replace(aws_iam_openid_connect_provider.cluster.url, "https://", "")

  workloads = {
    # The API server and its workers: objects, field-key wrapping.
    server = {
      namespace       = "erp"
      service_account = "erp-server"
      statements = [
        { Sid = "Objects", Effect = "Allow", Action = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"], Resource = "${aws_s3_bucket.objects.arn}/*" },
        { Sid = "ListObjects", Effect = "Allow", Action = "s3:ListBucket", Resource = aws_s3_bucket.objects.arn },
        { Sid = "ObjectKey", Effect = "Allow", Action = ["kms:Decrypt", "kms:GenerateDataKey"], Resource = aws_kms_key.data.arn },
        { Sid = "FieldMaster", Effect = "Allow", Action = ["kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"], Resource = aws_kms_key.fields.arn },
      ]
    }
    # The External Secrets Operator: the cell's secrets, read only.
    external_secrets = {
      namespace       = "external-secrets"
      service_account = "external-secrets"
      statements = [
        { Sid = "Secrets", Effect = "Allow", Action = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"], Resource = [for secret in aws_secretsmanager_secret.cell : secret.arn] },
        { Sid = "SecretsKey", Effect = "Allow", Action = "kms:Decrypt", Resource = aws_kms_key.data.arn },
      ]
    }
    # cert-manager's aws-privateca-issuer: certificates from the
    # environment's subordinate CA (T38).
    cert_manager = {
      namespace       = "cert-manager"
      service_account = "aws-privateca-issuer"
      statements = [
        { Sid = "Issue", Effect = "Allow", Action = ["acm-pca:IssueCertificate", "acm-pca:GetCertificate", "acm-pca:DescribeCertificateAuthority"], Resource = var.subordinate_ca_arn },
      ]
    }
    # The EBS CSI driver: the volumes of NATS JetStream and the like,
    # encrypted with the cluster key.
    ebs_csi = {
      namespace       = "kube-system"
      service_account = "ebs-csi-controller-sa"
      statements = [
        { Sid = "VolumeKey", Effect = "Allow", Action = ["kms:CreateGrant", "kms:ListGrants", "kms:RevokeGrant", "kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"], Resource = aws_kms_key.cluster.arn },
      ]
    }
  }
}

resource "aws_iam_role" "workload" {
  for_each = local.workloads
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
          "${local.oidc_host}:sub" = "system:serviceaccount:${each.value.namespace}:${each.value.service_account}"
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

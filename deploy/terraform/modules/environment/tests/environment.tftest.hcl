# An environment wired together against mock providers, in both shapes:
# the subordinate CA, the cell and the global tier form no cycle, the
# directory follows the cell's shape, the home cell reads the global
# tier's secrets, and the outputs are the charts' values. The modules' own
# tests check what each one builds.
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
  mock_resource "aws_acmpca_certificate_authority" {
    defaults = { arn = "arn:aws:acm-pca:eu-central-1:222222222222:certificate-authority/2222" }
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
  mock_resource "aws_iam_service_linked_role" {
    defaults = { arn = "arn:aws:iam::222222222222:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling" }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::222222222222:role/mock" }
  }
  mock_resource "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::erp-dev1-objects-x" }
  }
  mock_resource "aws_launch_template" {
    defaults = { id = "lt-0123456789abcdef0", latest_version = 1 }
  }
  mock_resource "aws_secretsmanager_secret" {
    defaults = { arn = "arn:aws:secretsmanager:eu-central-1:222222222222:secret:erp/dev1/x" }
  }
  mock_resource "aws_lb" {
    defaults = { arn = "arn:aws:elasticloadbalancing:eu-central-1:222222222222:loadbalancer/net/erp-dev-router/0" }
  }
  mock_resource "aws_lb_target_group" {
    defaults = { arn = "arn:aws:elasticloadbalancing:eu-central-1:222222222222:targetgroup/erp-dev-router/0" }
  }
}

mock_provider "aws" {
  alias = "tooling"
}

variables {
  environment        = "dev"
  cell               = "dev1"
  cidr               = "10.16.0.0/16"
  availability_zones = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
  admin_role_arns    = ["arn:aws:iam::222222222222:role/platform-admin"]
  tooling_account_id = "111111111111"
  root_ca_arn        = "arn:aws:acm-pca:eu-central-1:111111111111:certificate-authority/1111"
  root_certificate   = "-----BEGIN CERTIFICATE-----\nroot\n-----END CERTIFICATE-----"
  ca_organization    = "agentic-erp"
  ca_country         = "DE"
  zone_redundant     = false
  node_scaling       = { min = 2, desired = 2, max = 4 }
}

run "the_small_shape_plans_and_wires_its_parts" {
  command = apply

  assert {
    condition     = output.subordinate_ca_arn == "arn:aws:acm-pca:eu-central-1:222222222222:certificate-authority/2222"
    error_message = "the cell's issuer uses the environment's subordinate CA"
  }
  assert {
    condition     = output.cell.cluster_name == "erp-dev1" && output.cell.object_bucket != null && output.cell.cidr == "10.16.0.0/16"
    error_message = "the cell is named after the root's cell and its range is an output"
  }
  assert {
    condition     = toset(keys(output.cell.workload_role_arns)) == toset(["server", "external_secrets", "cert_manager", "ebs_csi", "load_balancer", "kyverno", "monitoring", "logging", "tracing", "analytics"])
    error_message = "every workload role the charts need is an output"
  }
  assert {
    condition     = output.cell.aws_endpoints.sts == "https://sts.eu-central-1.amazonaws.com"
    error_message = "the regional STS endpoint, which the VPC endpoint answers, is an output"
  }
  assert {
    condition     = output.global.router_target_group_arn == "arn:aws:elasticloadbalancing:eu-central-1:222222222222:targetgroup/erp-dev-router/0"
    error_message = "the router's target group is an output for the erp-global chart"
  }
  assert {
    condition     = output.global.web_target_group_arn == "arn:aws:elasticloadbalancing:eu-central-1:222222222222:targetgroup/erp-dev-router/0" && output.global.web_dns_name != null
    error_message = "the web workspace's target group and DNS name are outputs for the erp-global chart and its record (ADR-0052)"
  }
  assert {
    condition     = !module.global.directory_multi_az && module.global.directory_zone == "eu-central-1a"
    error_message = "the directory follows the cell's single-AZ shape, in its first zone"
  }
}

run "the_production_shape_is_zone_redundant" {
  command = apply
  # A state of its own: the shapes' databases differ, and neither may be
  # destroyed.
  state_key = "production"

  variables {
    environment    = "production"
    cell           = "prod1"
    cidr           = "10.64.0.0/16"
    zone_redundant = true
    node_scaling   = { min = 3, desired = 3, max = 12 }
  }

  assert {
    condition     = module.global.directory_multi_az
    error_message = "staging and production run the directory with a standby in another zone"
  }
  assert {
    condition     = output.cell.cluster_name == "erp-prod1"
    error_message = "the cell is named after the root's cell"
  }
}

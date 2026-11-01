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
  mock_data "aws_subnet" {
    defaults = { cidr_block = "10.10.0.0/24" }
  }
  mock_data "aws_rds_engine_version" {
    defaults = { version_actual = "17.6" }
  }
  mock_resource "aws_kms_key" {
    defaults = { arn = "arn:aws:kms:eu-central-1:222222222222:key/0000" }
  }
  mock_resource "aws_lb" {
    defaults = { arn = "arn:aws:elasticloadbalancing:eu-central-1:222222222222:loadbalancer/net/erp-dev-router/0" }
  }
  mock_resource "aws_lb_target_group" {
    defaults = { arn = "arn:aws:elasticloadbalancing:eu-central-1:222222222222:targetgroup/erp-dev-router/0" }
  }
}

# The cluster's private subnets, apart from the balancers' (mock_data's
# 10.10.0.0/24).
override_data {
  target = data.aws_subnet.private
  values = { cidr_block = "10.10.128.0/20" }
}

variables {
  environment               = "dev"
  admin_role_arns           = ["arn:aws:iam::222222222222:role/platform-admin"]
  vpc_id                    = "vpc-0123456789abcdef0"
  vpc_cidr                  = "10.10.0.0/16"
  private_subnet_ids        = ["subnet-1", "subnet-2", "subnet-3"]
  public_subnet_ids         = ["subnet-4", "subnet-5", "subnet-6"]
  cluster_security_group_id = "sg-0123456789abcdef0"
}

run "the_directory_requires_tls_1_3_and_survives_a_zone" {
  command = apply

  assert {
    condition     = { for p in aws_db_parameter_group.directory.parameter : p.name => p.value }["ssl_min_protocol_version"] == "TLSv1.3"
    error_message = "the directory accepts TLS 1.3 and nothing older (T38)"
  }
  assert {
    condition     = { for p in aws_db_parameter_group.directory.parameter : p.name => p.value }["rds.force_ssl"] == "1"
    error_message = "the directory refuses clients without TLS"
  }
  assert {
    condition     = aws_db_instance.directory.multi_az && aws_db_instance.directory.engine_version == "17.6"
    error_message = "PostgreSQL 17 with a standby in another zone"
  }
  assert {
    condition     = aws_db_instance.directory.storage_encrypted && aws_db_instance.directory.kms_key_id == aws_kms_key.directory.arn && aws_db_instance.directory.deletion_protection
    error_message = "encrypted with the directory's key and protected from deletion"
  }
}

run "the_load_balancer_passes_tls_through_to_the_router" {
  command = apply

  assert {
    condition     = aws_lb.router.load_balancer_type == "network" && !aws_lb.router.internal && aws_lb.router.enable_deletion_protection
    error_message = "an internet-facing NLB, protected from deletion"
  }
  assert {
    condition     = aws_lb_listener.https.port == 443 && aws_lb_listener.https.protocol == "TCP"
    error_message = "layer 4 on 443: the router terminates TLS, the load balancer holds no certificate"
  }
  assert {
    condition     = aws_lb_target_group.router.target_type == "ip" && aws_lb_target_group.router.port == 8081
    error_message = "the router's pods are the targets, on ERP_ROUTER_ADDR's port"
  }
  assert {
    condition     = aws_vpc_security_group_ingress_rule.router_pods.security_group_id == var.cluster_security_group_id && aws_vpc_security_group_ingress_rule.router_pods.from_port == 8081
    error_message = "the cluster lets the load balancer reach the router's port only"
  }
  assert {
    condition     = aws_lb_target_group.router.proxy_protocol_v2 && aws_lb_target_group.router.preserve_client_ip == "false"
    error_message = "each connection names its client in a PROXY v2 header and comes from the load balancer's own addresses (T40c5)"
  }
  assert {
    condition     = join(",", output.router_proxy_sources) == "10.10.0.0/24"
    error_message = "the router takes the header from the public subnets' ranges"
  }
}

run "the_web_workspace_has_its_own_load_balancer" {
  command = apply

  assert {
    condition     = aws_lb.web.load_balancer_type == "network" && !aws_lb.web.internal && aws_lb.web.enable_deletion_protection && aws_lb.web.name == "erp-dev-web"
    error_message = "an internet-facing NLB of its own, protected from deletion: a browser never meets the router's certificate request (ADR-0052)"
  }
  assert {
    condition     = aws_lb_listener.web_https.port == 443 && aws_lb_listener.web_https.protocol == "TCP"
    error_message = "layer 4 on 443: erp-web terminates TLS, the load balancer holds no certificate"
  }
  assert {
    condition     = aws_lb_target_group.web.target_type == "ip" && aws_lb_target_group.web.port == 8082 && aws_lb_target_group.web.proxy_protocol_v2 && aws_lb_target_group.web.preserve_client_ip == "false"
    error_message = "erp-web's pods are the targets, on ERP_WEB_ADDR's port, each connection with a PROXY v2 header from the balancer's addresses"
  }
  assert {
    condition     = aws_vpc_security_group_ingress_rule.web_pods.security_group_id == var.cluster_security_group_id && aws_vpc_security_group_ingress_rule.web_pods.from_port == 8082 && aws_vpc_security_group_ingress_rule.web_pods.to_port == 8082
    error_message = "the cluster lets the web load balancer reach erp-web's port only"
  }
  assert {
    condition     = join(",", output.web_proxy_sources) == "10.10.0.0/24"
    error_message = "erp-web takes the header from the public subnets' ranges"
  }
  assert {
    condition     = contains([for s in aws_secretsmanager_secret.global : s.name], "erp/global/web-public-certificate") && contains([for s in aws_secretsmanager_secret.global : s.name], "erp/global/web-public-key")
    error_message = "the web workspace's public certificate and key are entries under erp/global/"
  }
}

run "clients_can_be_limited" {
  command = plan
  variables {
    client_cidrs = ["203.0.113.0/24"]
  }
  assert {
    condition     = keys(aws_vpc_security_group_ingress_rule.clients) == ["203.0.113.0/24"] && keys(aws_vpc_security_group_ingress_rule.web_clients) == ["203.0.113.0/24"]
    error_message = "only the listed clients reach the load balancers"
  }
}

run "dev_and_test_run_the_directory_in_one_zone" {
  command = plan
  variables {
    directory_multi_az = false
    directory_zone     = "eu-central-1a"
  }
  assert {
    condition     = !aws_db_instance.directory.multi_az && aws_db_instance.directory.availability_zone == "eu-central-1a" && aws_db_instance.directory.storage_encrypted
    error_message = "one zone, the home cell's first, still encrypted"
  }
}

run "a_single_az_directory_needs_its_zone" {
  command = plan
  variables {
    directory_multi_az = false
  }
  expect_failures = [var.directory_zone]
}

run "the_routers_secrets_are_empty_entries_under_the_global_key" {
  command = apply

  assert {
    condition     = length(aws_secretsmanager_secret.global) == 8 && alltrue([for s in aws_secretsmanager_secret.global : s.kms_key_id == aws_kms_key.directory.arn && startswith(s.name, "erp/global/")])
    error_message = "the router's secrets are under erp/global/ and encrypted with the global tier's key"
  }
  assert {
    condition     = output.external_secrets.kms_key_arn == aws_kms_key.directory.arn && alltrue([for s in aws_secretsmanager_secret.global : contains(output.external_secrets.arns, s.arn)])
    error_message = "the home cell's External Secrets Operator is told every global secret and their key"
  }
}

run "pods_never_share_the_balancers_subnets" {
  command = plan
  override_data {
    target = data.aws_subnet.private
    values = { cidr_block = "10.10.0.0/16" }
  }
  expect_failures = [output.router_proxy_sources, output.web_proxy_sources]
}

run "the_web_workspaces_secrets_are_global" {
  command = apply
  assert {
    condition     = contains(keys(aws_secretsmanager_secret.global), "web-session-key") && contains(keys(aws_secretsmanager_secret.global), "web-session-key-previous") && aws_secretsmanager_secret.global["web-clients"].name == "erp/global/web-clients"
    error_message = "the web workspace's session keys, the current and the previous during a rotation, and client credentials are global secrets (T73b, T73c)"
  }
}

run "the_directory_logins_authenticate_with_iam" {
  command = apply
  assert {
    condition     = aws_db_instance.directory.iam_database_authentication_enabled
    error_message = "the directory takes IAM database authentication, for its clients' logins (ADR-0035 As built 2026-11-01)"
  }
  assert {
    condition = output.directory_users == {
      router = "arn:aws:rds-db:eu-central-1:222222222222:dbuser:${aws_db_instance.directory.resource_id}/erp_router_login"
      cell   = "arn:aws:rds-db:eu-central-1:222222222222:dbuser:${aws_db_instance.directory.resource_id}/erp_cell_login"
      web    = "arn:aws:rds-db:eu-central-1:222222222222:dbuser:${aws_db_instance.directory.resource_id}/erp_web_login"
    }
    error_message = "each client may connect as its own login on this directory alone"
  }
  assert {
    condition     = output.directory_region == "eu-central-1"
    error_message = "the tokens are signed for the directory's region"
  }
  assert {
    condition     = length([for name in keys(aws_secretsmanager_secret.global) : name if strcontains(name, "password")]) == 0
    error_message = "the directory's logins have no passwords to keep: no password secret is made"
  }
}

run "a_login_name_iam_cannot_match_is_refused" {
  command = plan
  variables {
    directory_logins = { router = "Erp_Router", cell = "erp_cell_login", web = "erp_web_login" }
  }
  expect_failures = [var.directory_logins]
}

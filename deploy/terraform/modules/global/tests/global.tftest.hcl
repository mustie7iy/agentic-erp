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
  mock_resource "aws_lb" {
    defaults = { arn = "arn:aws:elasticloadbalancing:eu-central-1:222222222222:loadbalancer/net/erp-dev-router/0" }
  }
  mock_resource "aws_lb_target_group" {
    defaults = { arn = "arn:aws:elasticloadbalancing:eu-central-1:222222222222:targetgroup/erp-dev-router/0" }
  }
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
}

run "clients_can_be_limited" {
  command = plan
  variables {
    client_cidrs = ["203.0.113.0/24"]
  }
  assert {
    condition     = keys(aws_vpc_security_group_ingress_rule.clients) == ["203.0.113.0/24"]
    error_message = "only the listed clients reach the load balancer"
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

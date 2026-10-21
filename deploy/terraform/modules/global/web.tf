# The web workspace's own internet-facing Network Load Balancer (ADR-0052
# §6): TCP 443 straight to erp-web's pods (ip targets), across the three
# zones, on its own hostname (app.<domain>), so a browser never meets the
# router's request for a client certificate. erp-web terminates TLS with
# the operators' public certificate (erp/global/web-public-*), as the
# router does; the health check is TCP on the traffic port.
#
# As for the router, each connection begins with a PROXY protocol v2
# header naming the client, which erp-web reads from the balancer's own
# addresses alone (ERP_WEB_PROXY_FROM: the public subnets,
# web_proxy_sources). The same clients may connect as to the router
# (client_cidrs).

resource "aws_security_group" "web" {
  name        = "${local.prefix}-web-lb"
  description = "The web workspace load balancer"
  vpc_id      = var.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "web_clients" {
  for_each          = toset(var.client_cidrs)
  security_group_id = aws_security_group.web.id
  description       = "HTTPS from clients"
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_egress_rule" "web" {
  security_group_id            = aws_security_group.web.id
  description                  = "To the web workspace pods"
  referenced_security_group_id = var.cluster_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = var.web_port
  to_port                      = var.web_port
}

resource "aws_vpc_security_group_ingress_rule" "web_pods" {
  security_group_id            = var.cluster_security_group_id
  description                  = "From the web workspace load balancer"
  referenced_security_group_id = aws_security_group.web.id
  ip_protocol                  = "tcp"
  from_port                    = var.web_port
  to_port                      = var.web_port
}

resource "aws_lb" "web" {
  name                             = "${local.prefix}-web"
  load_balancer_type               = "network"
  internal                         = false
  subnets                          = var.public_subnet_ids
  security_groups                  = [aws_security_group.web.id]
  enable_cross_zone_load_balancing = true
  enable_deletion_protection       = true
}

resource "aws_lb_target_group" "web" {
  name                 = "${local.prefix}-web"
  target_type          = "ip"
  protocol             = "TCP"
  port                 = var.web_port
  vpc_id               = var.vpc_id
  deregistration_delay = 30
  proxy_protocol_v2    = true
  preserve_client_ip   = false
  health_check {
    protocol            = "TCP"
    port                = "traffic-port"
    healthy_threshold   = 2
    unhealthy_threshold = 2
    interval            = 10
  }
}

resource "aws_lb_listener" "web_https" {
  load_balancer_arn = aws_lb.web.arn
  protocol          = "TCP"
  port              = 443
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.web.arn
  }
}

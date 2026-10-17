# The internet-facing Network Load Balancer: TCP 443 straight to the
# router's pods (ip targets), across the three zones. The health check is
# TCP on the traffic port, since the router's /livez sits behind its TLS.
#
# Each connection begins with a PROXY protocol v2 header naming the
# client, which the router reads from the balancer's own addresses alone
# (ERP_ROUTER_PROXY_FROM: the public subnets, router_proxy_sources).
# Client addresses are not preserved, so every connection to the router
# comes from those subnets and carries the header.

resource "aws_security_group" "router" {
  name        = "${local.prefix}-router-lb"
  description = "The router load balancer"
  vpc_id      = var.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "clients" {
  for_each          = toset(var.client_cidrs)
  security_group_id = aws_security_group.router.id
  description       = "HTTPS from clients"
  cidr_ipv4         = each.value
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

resource "aws_vpc_security_group_egress_rule" "router" {
  security_group_id            = aws_security_group.router.id
  description                  = "To the router pods"
  referenced_security_group_id = var.cluster_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = var.router_port
  to_port                      = var.router_port
}

resource "aws_vpc_security_group_ingress_rule" "router_pods" {
  security_group_id            = var.cluster_security_group_id
  description                  = "From the router load balancer"
  referenced_security_group_id = aws_security_group.router.id
  ip_protocol                  = "tcp"
  from_port                    = var.router_port
  to_port                      = var.router_port
}

resource "aws_lb" "router" {
  name                             = "${local.prefix}-router"
  load_balancer_type               = "network"
  internal                         = false
  subnets                          = var.public_subnet_ids
  security_groups                  = [aws_security_group.router.id]
  enable_cross_zone_load_balancing = true
  enable_deletion_protection       = true
}

resource "aws_lb_target_group" "router" {
  name                 = "${local.prefix}-router"
  target_type          = "ip"
  protocol             = "TCP"
  port                 = var.router_port
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

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.router.arn
  protocol          = "TCP"
  port              = 443
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.router.arn
  }
}

# The public subnets' ranges: the balancer's own addresses, the only ones
# the router takes a PROXY header from.
# (By count: the ids are unknown until the cell's subnets exist.)
data "aws_subnet" "public" {
  count = length(var.public_subnet_ids)
  id    = var.public_subnet_ids[count.index]
}

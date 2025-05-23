resource "aws_lb" "mirage-ecs" {
  name               = var.project
  internal           = false
  load_balancer_type = "application"
  security_groups = [
    aws_security_group.alb.id,
    aws_security_group.default.id,
  ]
  subnets = [
    data.aws_subnet.existing_a.id,
    data.aws_subnet.existing_c.id,
  ]
  tags = {
    Name = var.project
  }
}

resource "aws_lb_target_group" "mirage-ecs-http" {
  name                 = "${var.project}-http"
  port                 = 80
  target_type          = "ip"
  vpc_id               = data.aws_vpc.existing.id
  protocol             = "HTTP"
  deregistration_delay = 10

  health_check {
    path                = "/"
    port                = "traffic-port"
    protocol            = "HTTP"
    healthy_threshold   = 2
    unhealthy_threshold = 10
    timeout             = 5
    interval            = 6
  }
  tags = {
    Name = "${var.project}-http"
  }
}

resource "aws_lb_listener" "mirage-ecs-http" {
  load_balancer_arn = aws_lb.mirage-ecs.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"
    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
  tags = {
    Name = "${var.project}-https"
  }
}

resource "aws_lb_listener" "mirage-ecs-https" {
  load_balancer_arn = aws_lb.mirage-ecs.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = data.aws_acm_certificate.existing.arn

  default_action {
    type = "fixed-response"
    fixed_response {
      status_code  = "404"
      content_type = "text/plain"
      message_body = "404 Not Found"
    }
  }
  tags = {
    Name = "${var.project}-https"
  }
}

resource "aws_lb_listener_rule" "mirage-ecs-mirage-web" {
  listener_arn = aws_lb_listener.mirage-ecs-https.arn
  priority     = 1

  // If you want to use OIDC authentication, you need to set the following tf variables.
  // oauth_client_id, oauth_client_secret
  // You must set the OAuth callback URL to https://mirage.${var.domain}/oauth2/idpresponse
  // See also https://docs.aws.amazon.com/ja_jp/elasticloadbalancing/latest/application/listener-authenticate-users.html
  dynamic "action" {
    for_each = var.oauth_client_id != "" ? [1] : []
    content {
      type = "authenticate-oidc"
      authenticate_oidc {
        authorization_endpoint = jsondecode(data.http.oidc_configuration.response_body)["authorization_endpoint"]
        issuer                 = jsondecode(data.http.oidc_configuration.response_body)["issuer"]
        token_endpoint         = jsondecode(data.http.oidc_configuration.response_body)["token_endpoint"]
        user_info_endpoint     = jsondecode(data.http.oidc_configuration.response_body)["userinfo_endpoint"]
        scope                  = "email"
        client_id              = var.oauth_client_id
        client_secret          = var.oauth_client_secret
      }
    }
  }
  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.mirage-ecs-http.arn
  }

  condition {
    host_header {
      values = [
        "mirage-ecs-test.${var.domain}",
      ]
    }
  }
}

resource "aws_lb_listener_rule" "main-mirage-ecs-launched" {
  listener_arn = aws_lb_listener.mirage-ecs-https.arn
  priority     = 2
  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.mirage-ecs-http.arn
  }

  condition {
    host_header {
      values = [
        "*.mirage-ecs-test.${var.domain}",
      ]
    }
  }
}

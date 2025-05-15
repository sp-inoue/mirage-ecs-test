# 既存のホストゾーンを参照
data "aws_route53_zone" "existing" {
  name = var.domain
  # または zone_id = "Z1234567890ABC" のように指定
}

resource "aws_route53_record" "mirage-ecs" {
  zone_id = data.aws_route53_zone.existing.zone_id
  name    = "mirage-ecs-test.${var.domain}"
  type    = "A"
  alias {
    name                   = aws_lb.mirage-ecs.dns_name
    zone_id                = aws_lb.mirage-ecs.zone_id
    evaluate_target_health = true
  }
}

resource "aws_route53_record" "mirage-tasks" {
  zone_id = data.aws_route53_zone.existing.zone_id
  name    = "*.mirage-ecs-test.${var.domain}"
  type    = "A"
  alias {
    name                   = aws_lb.mirage-ecs.dns_name
    zone_id                = aws_lb.mirage-ecs.zone_id
    evaluate_target_health = true
  }
}

data "aws_acm_certificate" "existing" {
  domain      = "*.event-platform.jp"
  statuses    = ["ISSUED"]
  most_recent = true
}
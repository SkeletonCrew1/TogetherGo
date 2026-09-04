locals {
  # A wildcard SAN is validated by the DNS record of its parent domain, so
  # *.example.com and example.com share one record. Collapsing them here keeps
  # two Terraform resources from managing the same Route 53 record.
  #
  # These names are known at plan time, which is what makes the for_each below
  # legal: the record *values* come from the certificate and may be unknown, but
  # the keys must not be.
  validation_domains = var.create_certificate ? distinct([
    for name in concat([var.domain_name], var.subject_alternative_names) :
    trimprefix(name, "*.")
  ]) : []

  domain_validation_options = var.create_certificate ? {
    for dvo in aws_acm_certificate.this[0].domain_validation_options :
    trimprefix(dvo.domain_name, "*.") => dvo...
  } : {}
}

resource "aws_acm_certificate" "this" {
  count = var.create_certificate ? 1 : 0

  domain_name               = var.domain_name
  subject_alternative_names = var.subject_alternative_names
  validation_method         = "DNS"

  # A certificate cannot be deleted while a listener references it, so the
  # replacement has to exist before the old one goes away.
  lifecycle {
    create_before_destroy = true
  }

  tags = {
    Name = var.domain_name
  }
}

resource "aws_route53_record" "certificate_validation" {
  for_each = toset(local.validation_domains)

  zone_id = var.hosted_zone_id
  name    = local.domain_validation_options[each.key][0].resource_record_name
  type    = local.domain_validation_options[each.key][0].resource_record_type
  records = [local.domain_validation_options[each.key][0].resource_record_value]
  ttl     = 60

  # Re-issuing a certificate reuses the same record name; without this a rerun
  # fails on "record already exists".
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "this" {
  count = var.create_certificate ? 1 : 0

  certificate_arn         = aws_acm_certificate.this[0].arn
  validation_record_fqdns = [for record in aws_route53_record.certificate_validation : record.fqdn]
}

output "certificate_arn" {
  # aws_acm_certificate_validation is used (not the raw cert) so Terraform
  # waits for DNS validation to complete before the ALB listener is created.
  value = aws_acm_certificate_validation.this.certificate_arn
}

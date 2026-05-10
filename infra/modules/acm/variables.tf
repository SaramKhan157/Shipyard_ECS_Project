variable "domain_name" {
  description = "Fully-qualified domain name to issue the certificate for"
  type        = string
}

variable "route53_zone_id" {
  description = "Route 53 hosted zone ID for DNS validation"
  type        = string
}

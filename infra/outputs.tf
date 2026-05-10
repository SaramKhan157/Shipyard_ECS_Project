output "app_url" {
  description = "Live HTTPS URL of the deployed app"
  value       = "https://${aws_route53_record.app.name}"
}

output "ecr_repo_url" {
  description = "ECR repository URL for docker push"
  value       = module.ecr.repository_url
}

output "alb_dns_name" {
  description = "ALB DNS name (useful for debugging)"
  value       = module.alb.dns_name
}

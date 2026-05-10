variable "aws_region" {
  description = "AWS region for all resources"
  default     = "eu-north-1"
}

variable "domain_name" {
  description = "Root domain managed in Route 53, e.g. example.com"
  type        = string
}

variable "app_subdomain" {
  description = "Subdomain prefix for the app, e.g. 'shipyard' → shipyard.example.com"
  default     = "shipyard"
}

variable "image_tag" {
  description = "ECR image tag to deploy"
  default     = "latest"
}

variable "container_port" {
  description = "Port the container listens on"
  type        = number
  default     = 8080
}

variable "task_cpu" {
  description = "Fargate task CPU units"
  type        = number
  default     = 256
}

variable "task_memory" {
  description = "Fargate task memory in MB"
  type        = number
  default     = 512
}

variable "desired_count" {
  description = "Number of running ECS tasks"
  type        = number
  default     = 1
}

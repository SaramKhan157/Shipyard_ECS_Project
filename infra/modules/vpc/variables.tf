variable "name" {
  description = "Name prefix for all VPC resources"
  type        = string
}

variable "cidr" {
  description = "VPC CIDR block"
  default     = "10.0.0.0/16"
}

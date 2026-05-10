variable "name" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_ids" {
  type = list(string)
}

variable "acm_cert_arn" {
  type = string
}

variable "container_port" {
  type = number
}

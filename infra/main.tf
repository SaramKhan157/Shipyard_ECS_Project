locals {
  name = "shipyard"
  fqdn = "${var.app_subdomain}.${var.domain_name}"
}

data "aws_route53_zone" "this" {
  name         = var.domain_name
  private_zone = false
}

module "ecr" {
  source = "./modules/ecr"
  name   = local.name
}

module "vpc" {
  source = "./modules/vpc"
  name   = local.name
}

module "acm" {
  source          = "./modules/acm"
  domain_name     = local.fqdn
  route53_zone_id = data.aws_route53_zone.this.zone_id
}

module "alb" {
  source            = "./modules/alb"
  name              = local.name
  vpc_id            = module.vpc.vpc_id
  public_subnet_ids = module.vpc.public_subnet_ids
  acm_cert_arn      = module.acm.certificate_arn
  container_port    = var.container_port
}

module "ecs" {
  source           = "./modules/ecs"
  name             = local.name
  vpc_id           = module.vpc.vpc_id
  subnet_ids       = module.vpc.public_subnet_ids
  alb_sg_id        = module.alb.security_group_id
  target_group_arn = module.alb.target_group_arn
  ecr_repo_url     = module.ecr.repository_url
  image_tag        = var.image_tag
  container_port   = var.container_port
  task_cpu         = var.task_cpu
  task_memory      = var.task_memory
  desired_count    = var.desired_count
}

resource "aws_route53_record" "app" {
  zone_id = data.aws_route53_zone.this.zone_id
  name    = local.fqdn
  type    = "A"

  alias {
    name                   = module.alb.dns_name
    zone_id                = module.alb.zone_id
    evaluate_target_health = true
  }
}

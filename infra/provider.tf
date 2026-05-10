terraform {
  required_version = ">= 1.8"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }

  # Bootstrap (one-time, by hand) created the bucket + lock table.
  # See README "Bootstrap" section for the AWS CLI commands.
  backend "s3" {
    bucket         = "shipyard-tfstate-565856127049-eu-north-1"
    key            = "shipyard/terraform.tfstate"
    region         = "eu-north-1"
    dynamodb_table = "shipyard-tfstate-lock"
    encrypt        = true
  }
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project   = "shipyard"
      ManagedBy = "terraform"
    }
  }
}

# Proposed client example: the platformipam provider/API do not exist yet.
# Replace registry.example.com with the actual provider registry after publication.
terraform {
  required_version = ">= 1.6.0, < 2.0.0"

  required_providers {
    platformipam = {
      source  = "registry.example.com/platform/platformipam"
      version = "~> 0.1"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

variable "ipam_endpoint" {
  type        = string
  description = "Platform API origin, for example https://ipam.stage.example.com."
}

variable "aws_account_id" {
  type        = string
  description = "Authorized target AWS account; credentials must resolve to this account."
  validation {
    condition     = can(regex("^[0-9]{12}$", var.aws_account_id))
    error_message = "Use a 12-digit AWS account ID."
  }
}

variable "aws_region" {
  type    = string
  default = "eu-central-1"
}

variable "environment" {
  type    = string
  default = "prod"
}

variable "allocation_key" {
  type        = string
  default     = "prod-eu-central-1-orders"
  description = "Permanent identity; choose a new generation for replacement after release."
}

provider "platformipam" {
  endpoint = var.ipam_endpoint
  # Proposed credential source: PLATFORM_IPAM_TOKEN environment variable.
}

provider "aws" {
  region              = var.aws_region
  allowed_account_ids = [var.aws_account_id]
  # AWS credentials come from the standard workload identity/profile chain.
}

resource "platformipam_allocation" "vpc" {
  allocation_key = var.allocation_key
  scope          = "vpc"
  environment    = var.environment
  region         = var.aws_region
  account_id     = var.aws_account_id
  prefix_length  = 20
  description    = "Orders production VPC"
  labels         = { service = "orders" }

  timeouts = {
    create = "10m"
    update = "5m"
    delete = "5m"
  }
}

resource "aws_vpc" "orders" {
  cidr_block           = platformipam_allocation.vpc.cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name                           = "orders-${var.environment}"
    Environment                    = var.environment
    "platform-ipam:allocation-id"  = platformipam_allocation.vpc.id
    "platform-ipam:allocation-key" = platformipam_allocation.vpc.allocation_key
  }
}

output "allocation_id" {
  value = platformipam_allocation.vpc.id
}

output "vpc_cidr" {
  value = platformipam_allocation.vpc.cidr
}

output "vpc_id" {
  value = aws_vpc.orders.id
}

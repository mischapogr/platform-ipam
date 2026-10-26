terraform {
  required_version = ">= 1.14.0, < 2.0.0"

  required_providers {
    platformipam = {
      source = "registry.example.com/platform/platformipam"
    }
  }
}

variable "allocation_key" {
  type = string
}

provider "platformipam" {
  endpoint = "http://127.0.0.1:8080"
}

resource "platformipam_allocation" "replacement" {
  allocation_key = var.allocation_key
  scope          = "vpc"
  environment    = "development"
  region         = "eu-central-1"
  account_id     = "000000000000"
  prefix_length  = 22
  description    = "Synthetic operator beta"
}

output "allocation_id" {
  value = platformipam_allocation.replacement.id
}

output "cidr" {
  value = platformipam_allocation.replacement.cidr
}

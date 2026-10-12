terraform {

  required_providers {
    platformipam = {
      source  = "mischapogr/platformipam"
      version = "= 0.1.0-rc.1"
    }
  }
}

variable "platform_ipam_endpoint" {
  description = "HTTPS origin of the Platform-IPAM API. Replace the example before API operations."
  type        = string
  default     = "https://ipam.example.com"
}

provider "platformipam" {
  endpoint = var.platform_ipam_endpoint
}

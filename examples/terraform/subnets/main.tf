# Proposed extension: copy into the VPC example module as subnets.tf.
# Uses the providers, variables, allocation, and VPC defined in ../vpc/main.tf.
# Enable only after the subnet milestone; this directory is not a standalone module.
variable "private_subnet_zones" {
  description = "Stable logical subnet names mapped to authorized AWS AZ IDs."
  type        = map(string)
  default = {
    private_a = "euc1-az1"
    private_b = "euc1-az2"
  }
}

resource "platformipam_allocation" "private_subnet" {
  for_each = var.private_subnet_zones

  allocation_key       = "${var.allocation_key}/${each.key}-v1"
  scope                = "subnet"
  environment          = var.environment
  region               = var.aws_region
  account_id           = var.aws_account_id
  prefix_length        = 24
  parent_allocation_id = platformipam_allocation.vpc.id
  availability_zone_id = each.value
  description          = "Orders ${each.key} subnet"
  labels               = { service = "orders", tier = "private" }
}

resource "aws_subnet" "private" {
  for_each = var.private_subnet_zones

  vpc_id                  = aws_vpc.orders.id
  cidr_block              = platformipam_allocation.private_subnet[each.key].cidr
  availability_zone_id    = each.value
  map_public_ip_on_launch = false

  tags = {
    Name                           = "orders-${each.key}-${var.environment}"
    "platform-ipam:allocation-id"  = platformipam_allocation.private_subnet[each.key].id
    "platform-ipam:allocation-key" = platformipam_allocation.private_subnet[each.key].allocation_key
  }
}

output "private_subnet_cidrs" {
  value = { for key, allocation in platformipam_allocation.private_subnet : key => allocation.cidr }
}

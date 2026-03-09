variable "name_prefix" {
  description = "Name prefix for network resource tags."
  type        = string
}

variable "availability_zone" {
  description = "Availability zone for subnets. Leave blank to use first available AZ."
  type        = string
  default     = ""
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
}

variable "public_subnet_cidr" {
  description = "CIDR block for the public subnet used by the NAT gateway."
  type        = string
}

variable "private_subnet_cidr" {
  description = "CIDR block for the private host subnet."
  type        = string
}

variable "tags" {
  description = "Additional tags applied to created resources."
  type        = map(string)
  default     = {}
}

output "availability_zone" {
  description = "Availability zone used by the module."
  value       = local.selected_az
}

output "vpc_id" {
  description = "VPC ID."
  value       = aws_vpc.this.id
}

output "public_subnet_id" {
  description = "Public subnet ID used for NAT gateway placement."
  value       = aws_subnet.public.id
}

output "private_subnet_id" {
  description = "Private subnet ID used for private workloads."
  value       = aws_subnet.private.id
}

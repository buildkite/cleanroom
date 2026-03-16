output "vpc_id" {
  description = "VPC ID created for prod environment."
  value       = module.network.vpc_id
}

output "public_subnet_id" {
  description = "Public subnet ID containing NAT gateway."
  value       = module.network.public_subnet_id
}

output "private_subnet_id" {
  description = "Private subnet ID containing prod host."
  value       = module.network.private_subnet_id
}

output "instance_id" {
  description = "EC2 instance ID for prod host."
  value       = module.host.instance_id
}

output "private_ip" {
  description = "Private IP address for prod host."
  value       = module.host.private_ip
}

output "ssm_start_session_command" {
  description = "Command to open SSM session to prod host."
  value       = module.host.ssm_start_session_command
}

output "tailscale_ssh_pattern" {
  description = "Tailscale SSH pattern when tailscale auth key is configured."
  value       = module.host.tailscale_ssh_pattern
}

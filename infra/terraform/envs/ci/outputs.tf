output "vpc_id" {
  description = "VPC ID created for ci environment."
  value       = module.network.vpc_id
}

output "public_subnet_id" {
  description = "Public subnet ID containing NAT gateway."
  value       = module.network.public_subnet_id
}

output "private_subnet_id" {
  description = "Private subnet ID containing linux-ci host."
  value       = module.network.private_subnet_id
}

output "instance_id" {
  description = "EC2 instance ID for linux-ci host."
  value       = module.linux_ci.instance_id
}

output "private_ip" {
  description = "Private IP address for linux-ci host."
  value       = module.linux_ci.private_ip
}

output "ssm_start_session_command" {
  description = "Command to open SSM session to linux-ci host."
  value       = module.linux_ci.ssm_start_session_command
}

output "tailscale_ssh_pattern" {
  description = "Tailscale SSH pattern when tailscale auth key is configured."
  value       = module.linux_ci.tailscale_ssh_pattern
}

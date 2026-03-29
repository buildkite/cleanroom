output "vpc_id" {
  description = "VPC ID created for ci environment."
  value       = module.network.vpc_id
}

output "public_subnet_id" {
  description = "Public subnet ID containing NAT gateway."
  value       = module.network.public_subnet_id
}

output "private_subnet_id" {
  description = "Private subnet ID containing CI hosts."
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

output "mac_instance_id" {
  description = "EC2 instance ID for mac-ci host (null when disabled)."
  value       = var.enable_macos_ci ? module.mac_ci[0].instance_id : null
}

output "mac_private_ip" {
  description = "Private IP address for mac-ci host (null when disabled)."
  value       = var.enable_macos_ci ? module.mac_ci[0].private_ip : null
}

output "mac_ssm_start_session_command" {
  description = "Command to open SSM session to mac-ci host (null when disabled)."
  value       = var.enable_macos_ci ? module.mac_ci[0].ssm_start_session_command : null
}

output "mac_tailscale_ssh_pattern" {
  description = "Tailscale SSH pattern to connect to mac-ci host (null when disabled)."
  value       = var.enable_macos_ci ? module.mac_ci[0].tailscale_ssh_pattern : null
}

output "mac_dedicated_host_id" {
  description = "Dedicated host ID backing mac-ci instance (null when disabled)."
  value       = var.enable_macos_ci ? module.mac_ci[0].dedicated_host_id : null
}

output "mac_signer_instance_id" {
  description = "EC2 instance ID for the macOS signer host (null when disabled)."
  value       = var.enable_macos_signer_ci ? module.mac_signer[0].instance_id : null
}

output "mac_signer_private_ip" {
  description = "Private IP address for the macOS signer host (null when disabled)."
  value       = var.enable_macos_signer_ci ? module.mac_signer[0].private_ip : null
}

output "mac_signer_ssm_start_session_command" {
  description = "Command to open an SSM session to the macOS signer host (null when disabled)."
  value       = var.enable_macos_signer_ci ? module.mac_signer[0].ssm_start_session_command : null
}

output "mac_signer_tailscale_ssh_pattern" {
  description = "Tailscale SSH pattern to connect to the macOS signer host (null when disabled)."
  value       = var.enable_macos_signer_ci ? module.mac_signer[0].tailscale_ssh_pattern : null
}

output "mac_signer_dedicated_host_id" {
  description = "Dedicated host ID backing the macOS signer instance (null when disabled)."
  value       = var.enable_macos_signer_ci ? module.mac_signer[0].dedicated_host_id : null
}

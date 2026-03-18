output "instance_id" {
  description = "EC2 instance ID for the macOS host."
  value       = aws_instance.host.id
}

output "private_ip" {
  description = "Private IP address of the macOS host."
  value       = aws_instance.host.private_ip
}

output "ssm_start_session_command" {
  description = "Command to open an SSM session to the host."
  value       = "aws ssm start-session --region ${var.aws_region} --target ${aws_instance.host.id}"
}

output "tailscale_ssh_pattern" {
  description = "Tailscale SSH pattern when tailscale_auth_key_parameter_name is set."
  value       = trimspace(var.tailscale_auth_key_parameter_name) != "" ? "tailscale ssh ec2-user@${var.tailscale_hostname_prefix}-<instance-id>" : null
}

output "dedicated_host_id" {
  description = "EC2 dedicated host ID backing the macOS instance."
  value       = aws_ec2_host.mac.id
}

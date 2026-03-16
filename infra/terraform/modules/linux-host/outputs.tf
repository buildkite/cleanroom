output "instance_id" {
  description = "EC2 instance ID for the Linux host."
  value       = aws_instance.host.id
}

output "private_ip" {
  description = "Private IP address of the Linux host."
  value       = aws_instance.host.private_ip
}

output "ssm_start_session_command" {
  description = "Command to open an SSM session to the host."
  value       = "aws ssm start-session --region ${var.aws_region} --target ${aws_instance.host.id}"
}

output "tailscale_ssh_pattern" {
  description = "Tailscale SSH pattern when tailscale_auth_key_parameter_name is set."
  value       = "tailscale ssh root@${var.tailscale_hostname_prefix}-<instance-id>"
}

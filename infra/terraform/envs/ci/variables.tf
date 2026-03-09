variable "aws_region" {
  description = "AWS region for the host."
  type        = string
  default     = "ap-southeast-2"
}

variable "name_prefix" {
  description = "Name prefix for tags and resource names."
  type        = string
  default     = "cleanroom-ci"
}

variable "ami_id" {
  description = "Optional AMI override for linux-ci host. Leave empty to use latest Ubuntu AMI from SSM."
  type        = string
  default     = ""
}

variable "ubuntu_ami_ssm_parameter_name" {
  description = "SSM public parameter name for latest Ubuntu AMI."
  type        = string
  default     = "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
}

variable "instance_type" {
  description = "EC2 instance type for the host."
  type        = string
  default     = "m8i.large"
}

variable "root_volume_size_gib" {
  description = "Root EBS volume size in GiB."
  type        = number
  default     = 150
}

variable "buildkite_token_parameter_name" {
  description = "SSM SecureString parameter name storing the Buildkite token."
  type        = string
}

variable "tailscale_auth_key_parameter_name" {
  description = "Optional SSM SecureString parameter name storing Tailscale auth key."
  type        = string
  default     = ""
}

variable "git_deploy_key_parameter_name" {
  description = "Optional SSM SecureString parameter name storing an SSH deploy key for cloning."
  type        = string
  default     = ""
}

variable "repo_url" {
  description = "Git repository URL to clone for setup handoff."
  type        = string
  default     = "git@github.com:buildkite/cleanroom.git"
}

variable "repo_ref" {
  description = "Git ref checked out before running setup script."
  type        = string
  default     = "main"
}

variable "setup_script_path" {
  description = "Path to setup script in cloned repository."
  type        = string
  default     = "scripts/install.sh"
}

variable "tailscale_version" {
  description = "Tailscale version used for bootstrap when enabled."
  type        = string
  default     = "1.82.5"
}

variable "tailscale_hostname_prefix" {
  description = "Tailscale hostname prefix (<prefix>-<instance-id>)."
  type        = string
  default     = "cleanroom-ci-linux"
}

variable "tailscale_advertise_tags" {
  description = "Optional comma-separated tags passed to tailscale up --advertise-tags."
  type        = string
  default     = ""
}

variable "tailscale_enable_ssh" {
  description = "Enable tailscale up --ssh."
  type        = bool
  default     = true
}

variable "tailscale_accept_routes" {
  description = "Enable tailscale up --accept-routes."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Additional tags applied to created resources."
  type        = map(string)
  default     = {}
}

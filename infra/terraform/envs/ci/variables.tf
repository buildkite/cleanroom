variable "aws_region" {
  description = "AWS region for the host."
  type        = string
  default     = "us-west-2"
}

variable "name_prefix" {
  description = "Name prefix for tags and resource names."
  type        = string
  default     = "cleanroom-ci"
}

variable "availability_zone" {
  description = "Optional AZ override for CI subnets and host placement. Leave empty to use the first available AZ in-region."
  type        = string
  default     = ""
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
  description = "EC2 instance type for the host. Must support nested virtualization for Firecracker. Defaults to m8i.large for broad nested-virtualization availability; use a '*d' variant where your region/account supports nested virtualization on that type to place snapshots on local NVMe."
  type        = string
  default     = "m8i.large"
}

variable "root_volume_size_gib" {
  description = "Root EBS volume size in GiB."
  type        = number
  default     = 150
}

variable "enable_macos_ci" {
  description = "Create a private macOS CI instance (dedicated host + EC2 Mac)."
  type        = bool
  default     = false
}

variable "mac_ami_id" {
  description = "Optional AMI override for macOS CI host. Leave empty to use the Tahoe public SSM parameter that matches mac_instance_type."
  type        = string
  default     = ""
}

variable "mac_ami_ssm_parameter_name" {
  description = "Optional SSM public parameter name for the macOS CI host AMI. Leave empty to use the Tahoe public parameter that matches mac_instance_type."
  type        = string
  default     = ""
}

variable "mac_instance_type" {
  description = "EC2 instance type for macOS CI host. Must be a Mac metal type."
  type        = string
  default     = "mac2-m2.metal"

  validation {
    condition     = trimspace(var.mac_instance_type) != "" && endswith(var.mac_instance_type, ".metal")
    error_message = "mac_instance_type must be a non-empty Mac metal instance type (for example mac2-m2.metal)."
  }
}

variable "mac_root_volume_size_gib" {
  description = "Root EBS volume size in GiB for macOS CI host."
  type        = number
  default     = 200
}

variable "mac_buildkite_queue" {
  description = "Buildkite queue tag used by the macOS agent."
  type        = string
  default     = "cleanroom-mac"
}

variable "mac_setup_script_path" {
  description = "Path to macOS setup script in cloned repository."
  type        = string
  default     = "scripts/bootstrap-buildkite-agent-macos.sh"
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
  description = "SSM SecureString parameter name storing an SSH deploy key for cloning."
  type        = string

  validation {
    condition     = trimspace(var.git_deploy_key_parameter_name) != ""
    error_message = "git_deploy_key_parameter_name must be set."
  }
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
  default     = "scripts/bootstrap-buildkite-agent.sh"
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

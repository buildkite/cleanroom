variable "aws_region" {
  description = "AWS region for the host."
  type        = string
  default     = "us-west-2"
}

variable "name_prefix" {
  description = "Name prefix for tags and instance profile resources."
  type        = string
  default     = "cleanroom-ci-mac"
}

variable "vpc_id" {
  description = "VPC ID where the host security group is created."
  type        = string
}

variable "subnet_id" {
  description = "Private subnet ID where the host is launched."
  type        = string
}

variable "ami_id" {
  description = "macOS AMI ID for the host."
  type        = string

  validation {
    condition     = trimspace(var.ami_id) != ""
    error_message = "ami_id must be set for macos-ci host."
  }
}

variable "instance_type" {
  description = "EC2 instance type for the host (must be a Mac metal type)."
  type        = string
  default     = "mac2-m2.metal"

  validation {
    condition     = trimspace(var.instance_type) != "" && endswith(var.instance_type, ".metal")
    error_message = "instance_type must be a non-empty Mac metal instance type (for example mac2-m2.metal)."
  }
}

variable "root_volume_size_gib" {
  description = "Root EBS volume size in GiB."
  type        = number
  default     = 200
}

variable "buildkite_queue" {
  description = "Buildkite queue tag for this macOS host."
  type        = string
  default     = "cleanroom-mac"
}

variable "buildkite_token_parameter_name" {
  description = "SSM SecureString parameter name storing the Buildkite token."
  type        = string
}

variable "autologin_password_parameter_name" {
  description = "Optional SSM SecureString parameter name storing the macOS login password used to configure ec2-user auto-login when launchagent_mode is enabled."
  type        = string
  default     = ""
}

variable "launchagent_mode" {
  description = "When true, bootstrap the Buildkite agent as a logged-in user LaunchAgent instead of a system LaunchDaemon."
  type        = bool
  default     = false
}

variable "signer_mode" {
  description = "When true, install a Buildkite pre-command hook that restricts this host to signing jobs."
  type        = bool
  default     = false
}

variable "signer_require_signing_job" {
  description = "Require CLEANROOM_SIGNING_JOB=1 on signer hosts before allowing a job to run."
  type        = bool
  default     = true
}

variable "signer_allowed_branches" {
  description = "Comma-separated branch allowlist for signer hosts."
  type        = string
  default     = "main"
}

variable "signer_allowed_branch_prefixes" {
  description = "Optional comma-separated branch prefixes allowed on signer hosts."
  type        = string
  default     = ""
}

variable "signer_allow_tags" {
  description = "Allow tag builds on signer hosts."
  type        = bool
  default     = true
}

variable "signer_allow_pull_requests" {
  description = "Allow pull request builds on signer hosts."
  type        = bool
  default     = false
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
  description = "Path to the setup script inside the cloned repository."
  type        = string
  default     = "scripts/bootstrap-buildkite-agent-macos.sh"
}

variable "tailscale_hostname_prefix" {
  description = "Tailscale hostname prefix (<prefix>-<instance-id>) for the macOS host."
  type        = string
  default     = "cleanroom-ci-mac"
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

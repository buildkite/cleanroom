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

variable "mac_root_volume_encrypted" {
  description = "Whether the macOS CI host root EBS volume is encrypted."
  type        = bool
  default     = true
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

variable "mac_tailscale_hostname_prefix" {
  description = "Tailscale hostname prefix for macOS host (<prefix>-<instance-id>)."
  type        = string
  default     = "cleanroom-ci-mac"
}

variable "mac_autologin_password_parameter_name" {
  description = "Optional SSM SecureString parameter name storing the ec2-user password reused by the dedicated signer LaunchAgent when mac_signer_autologin_password_parameter_name is left empty."
  type        = string
  default     = ""
}

variable "enable_macos_signer_ci" {
  description = "Create a dedicated private macOS signer instance (separate host + queue) for signing/notarization jobs."
  type        = bool
  default     = false
}

variable "mac_signer_ami_id" {
  description = "Optional AMI override for the macOS signer host. Leave empty to use the Tahoe public SSM parameter that matches mac_signer_instance_type."
  type        = string
  default     = ""
}

variable "mac_signer_ami_ssm_parameter_name" {
  description = "Optional SSM public parameter name for the macOS signer host AMI. Leave empty to use the Tahoe public parameter that matches mac_signer_instance_type."
  type        = string
  default     = ""
}

variable "mac_signer_instance_type" {
  description = "Optional EC2 instance type for the macOS signer host. Leave empty to reuse mac_instance_type."
  type        = string
  default     = ""

  validation {
    condition     = trimspace(var.mac_signer_instance_type) == "" || endswith(var.mac_signer_instance_type, ".metal")
    error_message = "mac_signer_instance_type must be empty or a Mac metal instance type (for example mac2-m2.metal)."
  }
}

variable "mac_signer_root_volume_size_gib" {
  description = "Optional root EBS volume size in GiB for the macOS signer host. Use 0 to reuse mac_root_volume_size_gib."
  type        = number
  default     = 0
}

variable "mac_signer_root_volume_encrypted" {
  description = "Whether the macOS signer host root EBS volume is encrypted."
  type        = bool
  default     = true
}

variable "mac_signer_buildkite_queue" {
  description = "Buildkite queue tag used by the macOS signer agent."
  type        = string
  default     = "cleanroom-mac-signer"
}

variable "mac_signer_setup_script_path" {
  description = "Optional path to macOS signer setup script in cloned repository. Leave empty to reuse mac_setup_script_path."
  type        = string
  default     = ""
}

variable "mac_signer_tailscale_hostname_prefix" {
  description = "Tailscale hostname prefix for the macOS signer host (<prefix>-<instance-id>)."
  type        = string
  default     = "cleanroom-ci-mac-signer"
}

variable "mac_signer_autologin_password_parameter_name" {
  description = "Optional SSM SecureString parameter name storing the ec2-user password used to enable macOS auto-login for the signer LaunchAgent. Leave empty to reuse mac_autologin_password_parameter_name."
  type        = string
  default     = ""
}

variable "mac_signer_require_signing_job" {
  description = "Require CLEANROOM_SIGNING_JOB=1 on signer hosts before allowing a Buildkite job to run."
  type        = bool
  default     = true
}

variable "mac_signer_allowed_branches" {
  description = "Comma-separated branch allowlist enforced by the signer host pre-command hook."
  type        = string
  default     = "main"
}

variable "mac_signer_allowed_branch_prefixes" {
  description = "Optional comma-separated branch prefixes allowed by the signer host pre-command hook."
  type        = string
  default     = ""
}

variable "mac_signer_allow_tags" {
  description = "Allow tag builds on the signer host."
  type        = bool
  default     = true
}

variable "mac_signer_allow_pull_requests" {
  description = "Allow pull request builds on the signer host."
  type        = bool
  default     = false
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

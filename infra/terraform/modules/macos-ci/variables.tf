variable "aws_region" {
  description = "AWS region for the host."
  type        = string
  default     = "ap-southeast-2"
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
  default     = "mac2-m2pro.metal"

  validation {
    condition     = trimspace(var.instance_type) != "" && endswith(var.instance_type, ".metal")
    error_message = "instance_type must be a non-empty Mac metal instance type (for example mac2-m2pro.metal)."
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
  default     = "mac-small"
}

variable "buildkite_token_parameter_name" {
  description = "SSM SecureString parameter name storing the Buildkite token."
  type        = string
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

variable "tags" {
  description = "Additional tags applied to created resources."
  type        = map(string)
  default     = {}
}

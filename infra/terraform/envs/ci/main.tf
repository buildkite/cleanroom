data "aws_ssm_parameter" "ubuntu_ami" {
  name = var.ubuntu_ami_ssm_parameter_name
}

data "aws_ssm_parameter" "mac_ami" {
  count = var.enable_macos_ci && trimspace(var.mac_ami_id) == "" ? 1 : 0
  name  = local.selected_mac_ami_ssm_parameter_name
}

data "aws_ssm_parameter" "mac_signer_ami" {
  count = var.enable_macos_signer_ci && trimspace(var.mac_signer_ami_id) == "" ? 1 : 0
  name  = local.selected_mac_signer_ami_ssm_parameter_name
}

locals {
  selected_ami_id            = var.ami_id != "" ? var.ami_id : data.aws_ssm_parameter.ubuntu_ami.value
  selected_mac_instance_type = trimspace(var.mac_instance_type)
  selected_mac_ami_ssm_parameter_name = trimspace(var.mac_ami_ssm_parameter_name) != "" ? var.mac_ami_ssm_parameter_name : (
    startswith(local.selected_mac_instance_type, "mac1") ?
    "/aws/service/ec2-macos/tahoe/x86_64_mac/latest/image_id" :
    "/aws/service/ec2-macos/tahoe/arm64_mac/latest/image_id"
  )
  selected_mac_ami_id = !var.enable_macos_ci ? "" : (
    trimspace(var.mac_ami_id) != "" ? var.mac_ami_id : data.aws_ssm_parameter.mac_ami[0].value
  )
  selected_mac_signer_instance_type = trimspace(var.mac_signer_instance_type) != "" ? var.mac_signer_instance_type : local.selected_mac_instance_type
  selected_mac_signer_ami_ssm_parameter_name = trimspace(var.mac_signer_ami_ssm_parameter_name) != "" ? var.mac_signer_ami_ssm_parameter_name : (
    startswith(local.selected_mac_signer_instance_type, "mac1") ?
    "/aws/service/ec2-macos/tahoe/x86_64_mac/latest/image_id" :
    "/aws/service/ec2-macos/tahoe/arm64_mac/latest/image_id"
  )
  selected_mac_signer_ami_id = !var.enable_macos_signer_ci ? "" : (
    trimspace(var.mac_signer_ami_id) != "" ? var.mac_signer_ami_id : data.aws_ssm_parameter.mac_signer_ami[0].value
  )
  selected_mac_signer_root_volume_size_gib              = var.mac_signer_root_volume_size_gib > 0 ? var.mac_signer_root_volume_size_gib : var.mac_root_volume_size_gib
  selected_mac_signer_setup_script_path                 = trimspace(var.mac_signer_setup_script_path) != "" ? var.mac_signer_setup_script_path : var.mac_setup_script_path
  selected_mac_signer_autologin_password_parameter_name = trimspace(var.mac_signer_autologin_password_parameter_name) != "" ? var.mac_signer_autologin_password_parameter_name : var.mac_autologin_password_parameter_name

  # This env owns a fixed minimal private network shape.
  network = {
    availability_zone   = var.availability_zone
    vpc_cidr            = "10.42.0.0/24"
    public_subnet_cidr  = "10.42.0.0/26"
    private_subnet_cidr = "10.42.0.64/26"
  }
}

module "network" {
  source = "../../modules/network"

  name_prefix         = var.name_prefix
  availability_zone   = local.network.availability_zone
  vpc_cidr            = local.network.vpc_cidr
  public_subnet_cidr  = local.network.public_subnet_cidr
  private_subnet_cidr = local.network.private_subnet_cidr
  tags                = var.tags
}

module "linux_ci" {
  source = "../../modules/linux-host"

  aws_region                        = var.aws_region
  name_prefix                       = var.name_prefix
  vpc_id                            = module.network.vpc_id
  subnet_id                         = module.network.private_subnet_id
  ami_id                            = local.selected_ami_id
  instance_type                     = var.instance_type
  root_volume_size_gib              = var.root_volume_size_gib
  buildkite_token_parameter_name    = var.buildkite_token_parameter_name
  tailscale_auth_key_parameter_name = var.tailscale_auth_key_parameter_name
  git_deploy_key_parameter_name     = var.git_deploy_key_parameter_name
  repo_url                          = var.repo_url
  repo_ref                          = var.repo_ref
  setup_script_path                 = var.setup_script_path
  tailscale_version                 = var.tailscale_version
  tailscale_hostname_prefix         = var.tailscale_hostname_prefix
  tailscale_advertise_tags          = var.tailscale_advertise_tags
  tailscale_enable_ssh              = var.tailscale_enable_ssh
  tailscale_accept_routes           = var.tailscale_accept_routes
  tags                              = var.tags
}

module "mac_ci" {
  count  = var.enable_macos_ci ? 1 : 0
  source = "../../modules/macos-ci"

  aws_region                        = var.aws_region
  name_prefix                       = "${var.name_prefix}-mac"
  vpc_id                            = module.network.vpc_id
  subnet_id                         = module.network.private_subnet_id
  ami_id                            = local.selected_mac_ami_id
  instance_type                     = var.mac_instance_type
  root_volume_size_gib              = var.mac_root_volume_size_gib
  buildkite_queue                   = var.mac_buildkite_queue
  buildkite_token_parameter_name    = var.buildkite_token_parameter_name
  autologin_password_parameter_name = ""
  launchagent_mode                  = false
  tailscale_auth_key_parameter_name = var.tailscale_auth_key_parameter_name
  git_deploy_key_parameter_name     = var.git_deploy_key_parameter_name
  repo_url                          = var.repo_url
  repo_ref                          = var.repo_ref
  setup_script_path                 = var.mac_setup_script_path
  tailscale_hostname_prefix         = var.mac_tailscale_hostname_prefix
  tailscale_advertise_tags          = var.tailscale_advertise_tags
  tailscale_enable_ssh              = var.tailscale_enable_ssh
  tailscale_accept_routes           = var.tailscale_accept_routes
  tags                              = var.tags
}

module "mac_signer" {
  count  = var.enable_macos_signer_ci ? 1 : 0
  source = "../../modules/macos-ci"

  aws_region                        = var.aws_region
  name_prefix                       = "${var.name_prefix}-mac-signer"
  vpc_id                            = module.network.vpc_id
  subnet_id                         = module.network.private_subnet_id
  ami_id                            = local.selected_mac_signer_ami_id
  instance_type                     = local.selected_mac_signer_instance_type
  root_volume_size_gib              = local.selected_mac_signer_root_volume_size_gib
  buildkite_queue                   = var.mac_signer_buildkite_queue
  buildkite_token_parameter_name    = var.buildkite_token_parameter_name
  autologin_password_parameter_name = local.selected_mac_signer_autologin_password_parameter_name
  launchagent_mode                  = true
  signer_mode                       = true
  signer_require_signing_job        = var.mac_signer_require_signing_job
  signer_allowed_branches           = var.mac_signer_allowed_branches
  signer_allowed_branch_prefixes    = var.mac_signer_allowed_branch_prefixes
  signer_allow_tags                 = var.mac_signer_allow_tags
  signer_allow_pull_requests        = var.mac_signer_allow_pull_requests
  tailscale_auth_key_parameter_name = var.tailscale_auth_key_parameter_name
  git_deploy_key_parameter_name     = var.git_deploy_key_parameter_name
  repo_url                          = var.repo_url
  repo_ref                          = var.repo_ref
  setup_script_path                 = local.selected_mac_signer_setup_script_path
  tailscale_hostname_prefix         = var.mac_signer_tailscale_hostname_prefix
  tailscale_advertise_tags          = var.tailscale_advertise_tags
  tailscale_enable_ssh              = var.tailscale_enable_ssh
  tailscale_accept_routes           = var.tailscale_accept_routes
  tags                              = var.tags
}

data "aws_ssm_parameter" "ubuntu_ami" {
  name = var.ubuntu_ami_ssm_parameter_name
}

locals {
  selected_ami_id = var.ami_id != "" ? var.ami_id : data.aws_ssm_parameter.ubuntu_ami.value

  # This env owns a fixed private network shape separate from CI.
  network = {
    availability_zone   = var.availability_zone
    vpc_cidr            = "10.43.0.0/24"
    public_subnet_cidr  = "10.43.0.0/26"
    private_subnet_cidr = "10.43.0.64/26"
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

module "host" {
  source = "../../modules/linux-host"

  aws_region                        = var.aws_region
  name_prefix                       = var.name_prefix
  vpc_id                            = module.network.vpc_id
  subnet_id                         = module.network.private_subnet_id
  ami_id                            = local.selected_ami_id
  instance_type                     = var.instance_type
  root_volume_size_gib              = var.root_volume_size_gib
  tailscale_auth_key_parameter_name = var.tailscale_auth_key_parameter_name
  git_deploy_key_parameter_name     = var.git_deploy_key_parameter_name
  repo_url                          = var.repo_url
  repo_ref                          = var.repo_ref
  setup_script_path                 = var.setup_script_path
  cleanroom_version                 = var.cleanroom_version
  cleanroom_install_script_ref      = var.cleanroom_install_script_ref
  tailscale_version                 = var.tailscale_version
  tailscale_hostname_prefix         = var.tailscale_hostname_prefix
  tailscale_advertise_tags          = var.tailscale_advertise_tags
  tailscale_enable_ssh              = var.tailscale_enable_ssh
  tailscale_accept_routes           = var.tailscale_accept_routes
  tags                              = var.tags
}

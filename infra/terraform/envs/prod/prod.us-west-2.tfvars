aws_region  = "us-west-2"
name_prefix = "cleanroom-prod-usw2"

# Let AWS pick an AZ in-region.
availability_zone = ""

# Use latest Ubuntu AMI from SSM; keep instance type explicit.
ami_id               = ""
instance_type        = "m8i.4xlarge"
root_volume_size_gib = 500

tailscale_auth_key_parameter_name = "/tailscale/authkey/prod"
git_deploy_key_parameter_name     = "/buildkite/cleanroom/deploy-key"

repo_url                     = "git@github.com:buildkite/cleanroom.git"
repo_ref                     = "main"
setup_script_path            = "scripts/bootstrap-cleanroom-host.sh"
cleanroom_version            = "v0.4.0"
cleanroom_install_script_ref = "main"

tailscale_hostname_prefix = "cleanroom-prod-usw2-linux"
tailscale_advertise_tags  = ""
tailscale_enable_ssh      = true
tailscale_accept_routes   = false

tags = {
  env    = "prod"
  team   = "platform"
  region = "us-west-2"
}

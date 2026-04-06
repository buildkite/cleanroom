aws_region  = "ap-southeast-2"
name_prefix = "cleanroom-ci-apse2"

# Pin Sydney CI to the AZ where Mac capacity landed.
availability_zone = "ap-southeast-2b"

# Pin current live AMIs to avoid churn while the shared workspace stays aligned
# with the provisioned CI hosts.
ami_id        = "ami-0c33c6bd24cee108b"
instance_type = "m8i.large"

enable_macos_ci                  = true
enable_macos_signer_ci           = true
mac_ami_id                       = "ami-03410f84a69ea2482"
mac_instance_type                = "mac2-m2pro.metal"
mac_signer_ami_id                = "ami-03410f84a69ea2482"
mac_signer_root_volume_encrypted = false

buildkite_token_parameter_name    = "/buildkite/agent-token"
tailscale_auth_key_parameter_name = "/tailscale/authkey/ci"
git_deploy_key_parameter_name     = "/buildkite/cleanroom/deploy-key"

repo_url              = "git@github.com:buildkite/cleanroom.git"
repo_ref              = "main"
setup_script_path     = "scripts/bootstrap-buildkite-agent.sh"
mac_setup_script_path = "scripts/bootstrap-buildkite-agent-macos.sh"
mac_buildkite_queue   = "cleanroom-mac"

tailscale_hostname_prefix = "cleanroom-ci-apse2-linux"
tailscale_advertise_tags  = ""
tailscale_enable_ssh      = true
tailscale_accept_routes   = false

tags = {
  env    = "ci"
  team   = "platform"
  region = "ap-southeast-2"
}

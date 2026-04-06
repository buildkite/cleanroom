# Terraform Env: CI

Composition root for cleanroom CI infrastructure:

- `../../modules/network` for VPC/subnet/NAT
- `../../modules/linux-host` for the private Linux host bootstrap
- `../../modules/macos-ci` for optional private macOS host bootstrap

Default AMI behaviour:

- defaults to `us-west-2` for `aws_region`
- uses latest Ubuntu 24.04 AMI from SSM public parameter
- set `ami_id` in your selected var-file if you want to pin an explicit AMI
- set `enable_macos_ci = true` to enable the macOS host
- set `enable_macos_signer_ci = true` to enable a second dedicated macOS signer host
- macOS defaults to the latest Tahoe AMI from the public SSM parameter that matches `mac_instance_type`
- set `mac_ami_id` in your selected var-file if you want to pin an explicit macOS AMI
- set `mac_ami_ssm_parameter_name` if you want a different public SSM parameter than the Tahoe default

Network behaviour:

- env creates its own VPC/subnets via `modules/network`
- network CIDRs are fixed in env wiring
- `availability_zone` can be set to pin subnet and host placement to a specific AZ (useful for EC2 Mac capacity constraints)

Bootstrap behaviour:

- defaults to `scripts/bootstrap-buildkite-agent.sh`, which installs and starts a Buildkite agent for the `cleanroom` queue
- linux bootstrap installs a persistent rerunnable bootstrap command at `/usr/local/bin/cleanroom-bootstrap-linux` for in-place recovery via SSM
- macOS defaults to `scripts/bootstrap-buildkite-agent-macos.sh`, which keeps the `cleanroom-mac` queue on a system LaunchDaemon
- the optional signer host uses the same bootstrap script but defaults to the `cleanroom-mac-signer` queue in LaunchAgent mode with a signer-only `pre-command` hook
- set `mac_signer_autologin_password_parameter_name` to a SecureString parameter if you want bootstrap to configure `ec2-user` auto-login and start the signer agent in an Aqua session without manual login
- if `mac_signer_autologin_password_parameter_name` is empty, the signer host reuses `mac_autologin_password_parameter_name`
- set `mac_signer_autologin_password_parameter_name` if you want a different auto-login parameter for the signer host; otherwise it reuses the main macOS value
- when `tailscale_auth_key_parameter_name` is configured, macOS bootstrap also installs the open-source `tailscaled` daemon and enables Tailscale SSH
- macOS user-data installs a persistent rerunnable bootstrap command at `/usr/local/bin/cleanroom-bootstrap-macos` for in-place recovery via SSM
- override `setup_script_path` if you need custom host bootstrap logic
- override `mac_setup_script_path` if you need custom macOS host bootstrap logic
- override `mac_signer_setup_script_path` if you need custom signer host bootstrap logic

macOS dedicated host lifecycle:

- the dedicated host uses `prevent_destroy = true` to avoid accidental host churn
- macOS instance `user_data` changes are ignored to avoid replacement/stop-start cycles on dedicated hosts
- use SSM to rerun `/usr/local/bin/cleanroom-bootstrap-macos` instead of using `terraform apply -replace`

## Usage

```bash
cd infra/terraform/envs/ci
mise x -- terraform init
terraform workspace select -or-create ap-southeast-2
mise x -- terraform plan -var-file=ci.ap-southeast-2.tfvars
mise x -- terraform apply -var-file=ci.ap-southeast-2.tfvars
```

Available checked-in var-files:

- `ci.ap-southeast-2.tfvars` for the Sydney CI environment

`terraform.tfvars` remains ignored for local-only overrides and ad hoc envs. The
checked-in regional var-files are the shared source of truth for long-lived CI
workspaces.

## Access

After apply, use outputs:

- `ssm_start_session_command`
- `tailscale_ssh_pattern` (when tailscale auth key is configured)
- `mac_ssm_start_session_command` (when `enable_macos_ci` is true)
- `mac_tailscale_ssh_pattern` (when `enable_macos_ci` is true and tailscale auth key is configured)
- `mac_dedicated_host_id` (when `enable_macos_ci` is true)
- `mac_signer_ssm_start_session_command` (when `enable_macos_signer_ci` is true)
- `mac_signer_tailscale_ssh_pattern` (when `enable_macos_signer_ci` is true and tailscale auth key is configured)
- `mac_signer_dedicated_host_id` (when `enable_macos_signer_ci` is true)

## Linux Bootstrap Recovery (In-Place)

Rerun bootstrap on the existing Linux instance without replacing infrastructure:

- Hosts provisioned before this runner existed need one trusted bootstrap rerun to install `/usr/local/bin/cleanroom-bootstrap-linux`.

```bash
mise run ci:bootstrap:linux
```

Check bootstrap logs and the agent service:

```bash
mise run ci:bootstrap:linux:logs
```

Task defaults:

- `CLEANROOM_CI_AWS_PROFILE=buildkite-sandbox-pipelines-admin`
- `CLEANROOM_CI_AWS_REGION=ap-southeast-2`
- `CLEANROOM_CI_INSTANCE_ID` overrides Terraform lookup
- `CLEANROOM_CI_TERRAFORM_DIR=infra/terraform/envs/ci`

## macOS Bootstrap Recovery (In-Place)

Rerun bootstrap on the existing macOS instance without replacing infrastructure:

```bash
# Resolve current mac instance id from Terraform output
instance_id="$(mise x -- terraform -chdir=infra/terraform/envs/ci output -raw mac_instance_id)"

# Rerun bootstrap in place over SSM
AWS_PROFILE=buildkite-sandbox-pipelines-admin aws ssm send-command \
  --region ap-southeast-2 \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters '{"commands":["sudo env PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin /usr/local/bin/cleanroom-bootstrap-macos"]}'
```

After bootstrap, confirm the daemon-backed mac test agent is running:

```bash
AWS_PROFILE=buildkite-sandbox-pipelines-admin aws ssm send-command \
  --region ap-southeast-2 \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters '{"commands":["sudo launchctl print system/com.buildkite.agent.cleanroom-mac || true"]}'
```

The signer host uses the same rerunnable bootstrap command and recovery flow, but it runs in LaunchAgent mode. Swap `mac_instance_id` for `mac_signer_instance_id` and inspect `gui/<uid>/com.buildkite.agent.cleanroom-mac-signer`. If bootstrap logs `waiting for a GUI login session`, reboot after configuring the signer auto-login password parameter or log in once manually as `ec2-user`.

# Terraform Env: CI

Composition root for cleanroom CI infrastructure:

- `../../modules/network` for VPC/subnet/NAT
- `../../modules/linux-ci` for the private Linux host bootstrap
- `../../modules/macos-ci` for optional private macOS host bootstrap

Default AMI behaviour:

- uses latest Ubuntu 24.04 AMI from SSM public parameter
- set `ami_id` in `terraform.tfvars` if you want to pin an explicit AMI
- set `enable_macos_ci = true` and `mac_ami_id` to enable the macOS host

Network behaviour:

- env creates its own VPC/subnets via `modules/network`
- network CIDRs and AZ selection are fixed in env wiring (not user vars)

Bootstrap behaviour:

- defaults to `scripts/bootstrap-buildkite-agent.sh`, which installs and starts a Buildkite agent for the `cleanroom` queue
- macOS defaults to `scripts/bootstrap-buildkite-agent-macos.sh`, which installs and starts a Buildkite agent for the `cleanroom-mac` queue
- macOS user-data installs a persistent rerunnable bootstrap command at `/usr/local/bin/cleanroom-bootstrap-macos` for in-place recovery via SSM
- override `setup_script_path` if you need custom host bootstrap logic
- override `mac_setup_script_path` if you need custom macOS host bootstrap logic

macOS dedicated host lifecycle:

- the dedicated host uses `prevent_destroy = true` to avoid accidental host churn
- macOS instance `user_data` changes are ignored to avoid replacement/stop-start cycles on dedicated hosts
- use SSM to rerun `/usr/local/bin/cleanroom-bootstrap-macos` instead of using `terraform apply -replace`

## Usage

```bash
cd infra/terraform/envs/ci
cp terraform.tfvars.example terraform.tfvars
mise x -- terraform init
mise x -- terraform plan
mise x -- terraform apply
```

## Access

After apply, use outputs:

- `ssm_start_session_command`
- `tailscale_ssh_pattern` (when tailscale auth key is configured)
- `mac_ssm_start_session_command` (when `enable_macos_ci` is true)
- `mac_dedicated_host_id` (when `enable_macos_ci` is true)

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

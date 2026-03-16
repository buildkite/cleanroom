# Terraform Env: CI

Composition root for cleanroom CI infrastructure:

- `../../modules/network` for VPC/subnet/NAT
- `../../modules/linux-host` for the private Linux host bootstrap
- `../../modules/macos-ci` for optional private macOS host bootstrap

Default AMI behaviour:

- defaults to `us-west-2` for `aws_region`
- uses latest Ubuntu 24.04 AMI from SSM public parameter
- set `ami_id` in `terraform.tfvars` if you want to pin an explicit AMI
- set `enable_macos_ci = true` and `mac_ami_id` to enable the macOS host

Network behaviour:

- env creates its own VPC/subnets via `modules/network`
- network CIDRs are fixed in env wiring
- `availability_zone` can be set to pin subnet and host placement to a specific AZ (useful for EC2 Mac capacity constraints)

Bootstrap behaviour:

- defaults to `scripts/bootstrap-buildkite-agent.sh`, which installs and starts a Buildkite agent for the `cleanroom` queue
- linux bootstrap installs a persistent rerunnable bootstrap command at `/usr/local/bin/cleanroom-bootstrap-linux` for in-place recovery via SSM
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

## Linux Bootstrap Recovery (In-Place)

Rerun bootstrap on the existing Linux instance without replacing infrastructure:

- Hosts provisioned before this runner existed need one trusted bootstrap rerun to install `/usr/local/bin/cleanroom-bootstrap-linux`.

```bash
instance_id="$(mise x -- terraform -chdir=infra/terraform/envs/ci output -raw instance_id)"

AWS_PROFILE=buildkite-sandbox-pipelines-admin aws ssm send-command \
  --region us-west-2 \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters '{"commands":["sudo env PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin /usr/local/bin/cleanroom-bootstrap-linux"]}'
```

Check bootstrap logs and the agent service:

```bash
AWS_PROFILE=buildkite-sandbox-pipelines-admin aws ssm send-command \
  --region us-west-2 \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters '{"commands":["sudo tail -n 120 /var/log/cleanroom-bootstrap-linux.log","sudo systemctl status buildkite-agent@cleanroom.service --no-pager","sudo tail -n 120 /var/lib/buildkite-agent/logs/buildkite-agent-cleanroom.log"]}'
```

## macOS Bootstrap Recovery (In-Place)

Rerun bootstrap on the existing macOS instance without replacing infrastructure:

```bash
# Resolve current mac instance id from Terraform output
instance_id="$(mise x -- terraform -chdir=infra/terraform/envs/ci output -raw mac_instance_id)"

# Rerun bootstrap in place over SSM
AWS_PROFILE=buildkite-sandbox-pipelines-admin aws ssm send-command \
  --region us-west-2 \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters '{"commands":["sudo env PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin /usr/local/bin/cleanroom-bootstrap-macos"]}'
```

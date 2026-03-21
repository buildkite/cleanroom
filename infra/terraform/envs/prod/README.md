# Terraform Env: Prod

Composition root for a private Linux Cleanroom host intended for operator-driven
production work:

- `../../modules/network` for VPC/subnet/NAT
- `../../modules/linux-host` for the private Linux host bootstrap handoff

Default host behaviour:

- requires an explicit region-specific var-file
- uses latest Ubuntu 24.04 AMI from SSM public parameter
- defaults to `m8i.4xlarge` with a 500 GiB root volume
- installs `scripts/bootstrap-cleanroom-host.sh`, which installs the pinned
  `cleanroom` GitHub release, installs the matching
  `/usr/local/sbin/cleanroom-root-helper`, installs Firecracker, writes runtime
  config, installs a rerunnable host bootstrap command, and installs the system
  daemon
  Set `cleanroom_release_repo` when `repo_url` is not a GitHub remote.

Network behaviour:

- env creates its own VPC/subnets via `modules/network`
- network CIDRs are fixed in env wiring and separate from CI
- `availability_zone` can be set to pin subnet and host placement to a
  specific AZ

Runtime behaviour:

- daemon runs as a systemd service on the root-owned system socket
- access the host over SSM or Tailscale, then use `sudo cleanroom ...`
- prod sets `user_data_replace_on_change = false`, so bootstrap changes do not
  force EC2 replacement and host software upgrades are rerun in-place instead
- the rerunnable host updater reloads mutable settings from the checked-out
  region var-file on each run before reinstalling cleanroom
- runtime config enables Firecracker snapshots with the file driver
- when the bootstrap-created ZFS dataset is available, snapshots live under the
  dataset mountpoint; otherwise they fall back to `/var/lib/cleanroom/snapshots`
- default guest sizing is 4 vCPU / 8192 MiB for interactive work

## Usage

```bash
cd infra/terraform/envs/prod
mise x -- terraform init
terraform workspace select -or-create ap-southeast-2
mise x -- terraform plan -var-file=prod.ap-southeast-2.tfvars
mise x -- terraform apply -var-file=prod.ap-southeast-2.tfvars
```

Available checked-in var-files:

- `prod.ap-southeast-2.tfvars` for the Sydney prod host
- `prod.us-west-2.tfvars` for the us-west-2 prod host

`terraform.tfvars` is intentionally left empty so the selected workspace must be
paired with an explicit `-var-file=...`.

## Access

After apply, use outputs:

- `ssm_start_session_command`
- `tailscale_ssh_pattern` (when tailscale auth key is configured)

Once connected to the host:

```bash
sudo cleanroom doctor
sudo cleanroom daemon status
sudo cleanroom exec -- <command>
sudo /usr/local/bin/cleanroom-bootstrap-host
```

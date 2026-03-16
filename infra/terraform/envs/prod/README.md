# Terraform Env: Prod

Composition root for a private Linux Cleanroom host intended for operator-driven
production work:

- `../../modules/network` for VPC/subnet/NAT
- `../../modules/linux-host` for the private Linux host bootstrap handoff

Default host behaviour:

- defaults to `us-west-2` for `aws_region`
- uses latest Ubuntu 24.04 AMI from SSM public parameter
- defaults to `m8i.4xlarge` with a 500 GiB root volume
- installs `scripts/bootstrap-cleanroom-host.sh`, which builds `cleanroom` from
  the checked-out repo/ref, installs Firecracker, writes runtime config, and
  installs the system daemon

Network behaviour:

- env creates its own VPC/subnets via `modules/network`
- network CIDRs are fixed in env wiring and separate from CI
- `availability_zone` can be set to pin subnet and host placement to a
  specific AZ

Runtime behaviour:

- daemon runs as a systemd service on the root-owned system socket
- access the host over SSM or Tailscale, then use `sudo cleanroom ...`
- runtime config enables Firecracker snapshots with the file driver
- when the bootstrap-created ZFS dataset is available, snapshots live under the
  dataset mountpoint; otherwise they fall back to `/var/lib/cleanroom/snapshots`
- default guest sizing is 4 vCPU / 8192 MiB for interactive work

## Usage

```bash
cd infra/terraform/envs/prod
cp terraform.tfvars.example terraform.tfvars
mise x -- terraform init
mise x -- terraform plan
mise x -- terraform apply
```

## Access

After apply, use outputs:

- `ssm_start_session_command`
- `tailscale_ssh_pattern` (when tailscale auth key is configured)

Once connected to the host:

```bash
sudo cleanroom doctor
sudo cleanroom daemon status
sudo cleanroom exec -- <command>
```

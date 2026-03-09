# Terraform Env: CI

Composition root for cleanroom CI infrastructure:

- `../../modules/network` for VPC/subnet/NAT
- `../../modules/linux-ci` for the private Linux host bootstrap

Default AMI behaviour:

- uses latest Ubuntu 24.04 AMI from SSM public parameter
- set `ami_id` in `terraform.tfvars` if you want to pin an explicit AMI

Network behaviour:

- env creates its own VPC/subnets via `modules/network`
- network CIDRs and AZ selection are fixed in env wiring (not user vars)

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

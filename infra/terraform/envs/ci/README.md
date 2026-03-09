# Terraform Env: CI

Composition root for cleanroom CI infrastructure:

- `../../modules/network` for VPC/subnet/NAT
- `../../modules/linux-ci` for the private Linux host bootstrap

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

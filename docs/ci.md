# CI Setup (Buildkite)

This repository uses Buildkite for CI with three queues:

- `hosted`: Linux unit/integration tests (`go test ./...`)
- `cleanroom-mac`: macOS unit/integration tests (`go test ./...`) and `darwin-vz` end-to-end checks (`scripts/ci-darwin-vz-e2e.sh`)
- `cleanroom`: Linux Firecracker end-to-end checks (`scripts/ci-cleanroom-e2e.sh`)

Pipeline config lives in `.buildkite/pipeline.yml`.

## 1. Create/Configure Pipeline

1. Create a Buildkite pipeline for this repository.
2. Ensure the pipeline reads `.buildkite/pipeline.yml` from the repo.
3. Ensure all required queues are available:
- `hosted`
- `cleanroom-mac`
- `cleanroom`

## 2. Hosted and macOS Queues

No special setup is required beyond a working Buildkite agent image and internet access.

For self-hosted macOS capacity, Terraform can provision a private EC2 Mac host and dedicated host via `infra/terraform/envs/ci` (`enable_macos_ci = true`). By default it resolves the latest Tahoe AMI from the AWS public SSM parameter that matches `mac_instance_type`, and you can still override that with `mac_ami_id` or `mac_ami_ssm_parameter_name`. This keeps mac queue access private-only (SSM, no inbound public rules).

Notes:

- `mise` is bootstrapped per-step via the pinned `lox/mise-buildkite-plugin` reference in `.buildkite/pipeline.yml`.
- Self-hosted agents need `curl` and `tar` available so the plugin can install `mise`.
- Per-step cache is enabled for `hosted` and `cleanroom-mac` steps.
- Avoid global pipeline `cache:` blocks if self-hosted queues are present.

### 2.1 macOS bootstrap updates and recovery

EC2 Mac dedicated hosts should be treated as long-lived capacity.

- Avoid `terraform apply -replace=module.mac_ci[0].aws_instance.host` for bootstrap-only changes.
- Use in-place SSM reruns against the existing instance instead.
- Host-level safeguards in Terraform keep the dedicated host stable and avoid user-data replacement churn.

Rerun bootstrap in-place:

```bash
instance_id="$(mise x -- terraform -chdir=infra/terraform/envs/ci output -raw mac_instance_id)"

AWS_PROFILE=buildkite-sandbox-pipelines-admin aws ssm send-command \
  --region ap-southeast-2 \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters '{"commands":["sudo env PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin /usr/local/bin/cleanroom-bootstrap-macos"]}'
```

Check bootstrap logs and agent service:

```bash
AWS_PROFILE=buildkite-sandbox-pipelines-admin aws ssm send-command \
  --region ap-southeast-2 \
  --instance-ids "$instance_id" \
  --document-name AWS-RunShellScript \
  --parameters '{"commands":["sudo tail -n 120 /var/log/cleanroom-bootstrap-macos.log","sudo launchctl print system/com.buildkite.agent.cleanroom-mac","sudo tail -n 120 /var/lib/buildkite-agent/logs/buildkite-agent-cleanroom-mac.log","sudo tail -n 120 /var/lib/buildkite-agent/logs/buildkite-agent-cleanroom-mac.error.log"]}'
```

## 3. cleanroom-mac Queue (darwin-vz E2E)

The `:apple: E2E (darwin-vz)` step runs launched execution checks on macOS using `Virtualization.framework`.

### 3.1 Required host capabilities

- macOS host with `Virtualization.framework` available
- `mkfs.ext4` and `debugfs` available (`brew install e2fsprogs`)
- ability to build and ad-hoc sign `cleanroom-darwin-vz` during the CI step
- internet egress to pull `sandbox.image.ref` on first run

Notes:

- `scripts/ci-darwin-vz-e2e.sh` builds `dist/cleanroom` and `dist/cleanroom-darwin-vz.app`, exports `CLEANROOM_DARWIN_VZ_HELPER` to the built helper, and isolates XDG runtime paths.
- the CI script writes `backends.darwin-vz.network.mode: filehandle` into its temporary config so CI exercises the supported darwin-vz network path.
- the script also builds `dist/cleanroom-guest-agent-linux-<host-arch>` so CI can self-bootstrap the Linux guest agent dependency without a separate install step.
- Set `CLEANROOM_DARWIN_VZ_KERNEL_IMAGE` on the worker if you want an explicit kernel path; otherwise the script uses managed-kernel fallback.

### 3.2 Notarized macOS release pkg step

The `:package: macOS release pkg` step runs on the dedicated
`cleanroom-mac-signer` queue for:

- tagged builds (`v*`)
- the current notarized-release development branch (`codex/macos-notarized-release-pkg`)

It builds both `arm64` and `x86_64` helper bundles, packages them into signed
installer pkgs, notarizes them, and uploads the per-arch Darwin release
directories as compressed Buildkite artifacts alongside the `.pkg` plus
`.sha256` files.

Required Buildkite cluster secrets:

- `CLEANROOM_MACOS_RELEASE_HELPER_CERT_P12_BASE64`
- `CLEANROOM_MACOS_RELEASE_HELPER_CERT_PASSWORD`
- `CLEANROOM_MACOS_RELEASE_HELPER_PROVISION_PROFILE_BASE64`
- `CLEANROOM_MACOS_RELEASE_HELPER_SIGN_IDENTITY`
- `CLEANROOM_MACOS_INSTALLER_CERT_P12_BASE64`
- `CLEANROOM_MACOS_INSTALLER_CERT_PASSWORD`
- `CLEANROOM_MACOS_INSTALLER_SIGN_IDENTITY`
- `CLEANROOM_MACOS_NOTARY_KEY_P8_BASE64`
- `CLEANROOM_MACOS_NOTARY_KEY_ID`
- `CLEANROOM_MACOS_NOTARY_ISSUER_ID`

Notes:

- The signer queue expects `CLEANROOM_SIGNING_JOB=1` and should restrict allowed
  branches/tags at the agent hook layer. The Buildkite step sets that env var so
  signer hosts can reject non-signing jobs by default.
- `scripts/ci-macos-release-pkg.sh` imports the two Developer ID identities into
  a temporary user keychain for the job, so it does not rely on preinstalled
  host keychain state.
- Scope the release/notary secrets to the `cleanroom` pipeline and the
  `cleanroom-mac-signer` queue rather than the general `cleanroom-mac` queue.
- The step uses helper bundle identifier `com.buildkite.cleanroom.darwin-vz`.
- Branch builds derive a synthetic package version from the Buildkite build
  number (`0.0.<build>`). Tag builds use the tag version without the leading
  `v`.

### 3.3 Buildkite tag release publishing

Tagged builds fan in to a hosted `:rocket: Publish release` step after the test
and signer work completes.

That step:

- downloads the `release-extra/darwin_*.tar.gz` artifacts from the signer queue
- rebuilds the Linux guest-agent release extras
- runs `goreleaser release --clean`
- uploads the notarized macOS `.pkg` assets to the same GitHub release

Required Buildkite cluster secret:

- `CLEANROOM_GITHUB_RELEASE_TOKEN`

The token must be able to create/update GitHub releases for
`buildkite/cleanroom`.

## 4. Cleanroom Queue (Firecracker E2E)

The `:fire: E2E (Firecracker)` step runs a real launched Firecracker execution and needs host preparation.

### 4.1 Required host capabilities

- Linux host with `/dev/kvm` available
- Firecracker binary (default `/usr/local/bin/firecracker`)
- Readable kernel image for the `buildkite-agent` user (or allow managed kernel auto-download)
- Internet egress to pull `sandbox.image.ref` from registry on first run
- `mkfs.ext4` available for OCI-to-ext4 materialization
- Passwordless sudo for required network setup commands

### 4.2 Place runtime kernel image

Put kernel assets under the Buildkite agent home so CI can read them:

```bash
sudo install -d -o buildkite-agent -g buildkite-agent /var/lib/buildkite-agent/.local/share/cleanroom/images
sudo cp /path/to/vmlinux.bin /var/lib/buildkite-agent/.local/share/cleanroom/images/
sudo chown buildkite-agent:buildkite-agent /var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin
```

The Firecracker backend derives runtime rootfs from `sandbox.image.ref` automatically; no prebuilt `rootfs.ext4` is required.

The pipeline is currently configured to use:

- `CLEANROOM_KERNEL_IMAGE=/var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin`
- `CLEANROOM_FIRECRACKER_BINARY=/usr/local/bin/firecracker`

### 4.2.1 Linux bootstrap updates and recovery

Linux cleanroom hosts should be treated as long-lived capacity.

- Avoid `terraform apply` just to roll the helper or bootstrap scripts forward.
- Use the host-owned bootstrap runner instead.
- Hosts provisioned before this runner existed need one trusted bootstrap rerun to install `/usr/local/bin/cleanroom-bootstrap-linux`.

Rerun bootstrap in-place:

```bash
mise run ci:bootstrap:linux
```

Check bootstrap logs and agent service:

```bash
mise run ci:bootstrap:linux:logs
```

Task defaults:

- `CLEANROOM_CI_AWS_PROFILE=buildkite-sandbox-pipelines-admin`
- `CLEANROOM_CI_AWS_REGION=ap-southeast-2`
- `CLEANROOM_CI_INSTANCE_ID` overrides Terraform lookup
- `CLEANROOM_CI_TERRAFORM_DIR=infra/terraform/envs/ci`

### 4.3 Privileged helper execution

Firecracker always executes privileged host operations through a single root-owned helper.

Runtime config key:

- `backends.firecracker.privileged_helper_path`

For CI script usage, you can also set:

- `CLEANROOM_PRIVILEGED_HELPER_PATH`

Install the helper from this repository and only grant sudo access to that helper:

```bash
sudo install -o root -g root -m 0755 scripts/cleanroom-root-helper.sh /usr/local/sbin/cleanroom-root-helper
```

```sudoers
buildkite-agent ALL=(root) NOPASSWD: /usr/local/sbin/cleanroom-root-helper *
```

Then set `CLEANROOM_PRIVILEGED_HELPER_PATH=/usr/local/sbin/cleanroom-root-helper` if you need to override the runtime config.

`scripts/ci-cleanroom-e2e.sh` probes the installed helper with `capabilities`, and `cleanroom doctor` also records the helper `version`, before running Firecracker checks. They do not compare helper file hashes and they do not self-update the helper from the checkout.

If a branch needs a new privileged helper capability, roll out the updated helper on the CI host first, then rerun the branch. The normal path is:

1. Merge the helper change to `main`.
2. Rerun trusted host provisioning, for example `scripts/bootstrap-buildkite-agent.sh` via SSM.
3. Rerun dependent PR builds once the host helper has been updated.

## 5. Optional Agent Environment Hook

If you prefer host-level env over pipeline step env, set variables in `/etc/buildkite-agent/hooks/environment`.

```bash
#!/usr/bin/env bash
set -euo pipefail

export CLEANROOM_KERNEL_IMAGE="/var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin"
export CLEANROOM_FIRECRACKER_BINARY="/usr/local/bin/firecracker"
```

## 6. Collision Safety

`scripts/ci-cleanroom-e2e.sh` and `scripts/ci-darwin-vz-e2e.sh` isolate CI runtime paths using temporary XDG directories (`XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, `XDG_STATE_HOME`, `XDG_RUNTIME_DIR`, `XDG_DATA_HOME`) and a job-local unix socket.

This prevents collisions with any long-running cleanroom instance on the same host.

## 7. Verification

After setup:

1. Trigger a build.
2. Confirm `:test_tube: Test (Linux)` and `:test_tube: Test (macOS)` pass.
3. Confirm `:apple: E2E (darwin-vz)` passes doctor, launched execution, exit-code, and policy checks.
4. Confirm `:fire: E2E (Firecracker)` passes doctor, launch, exec, persistent sandbox lifecycle, and observability checks.

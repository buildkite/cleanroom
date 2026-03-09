# CI Setup (Buildkite)

This repository uses Buildkite for CI with three queues:

- `hosted`: Linux unit/integration tests (`mise run test`)
- `mac-small`: macOS unit/integration tests (`mise run test`)
- `cleanroom`: Linux Firecracker end-to-end checks (`scripts/ci-cleanroom-e2e.sh`)

Pipeline config lives in `.buildkite/pipeline.yml`.

## 1. Create/Configure Pipeline

1. Create a Buildkite pipeline for this repository.
2. Ensure the pipeline reads `.buildkite/pipeline.yml` from the repo.
3. Ensure all required queues are available:
- `hosted`
- `mac-small`
- `cleanroom`

## 2. Provision Hosts With CloudFormation

Use the stack template at `infra/cloudformation/ci-hosts.yaml` to create a reproducible Linux CI worker.

What the stack provisions:

- dedicated VPC, subnet, IGW, and an egress-only security group (no inbound SSH rule)
- Linux host via Auto Scaling Group (`desired=1`) for self-healing replacement
- IAM instance profile with least-privilege access to your Buildkite token in SSM Parameter Store
- optional Tailscale bootstrap on Linux (including `tailscale up --ssh`)
- host bootstrap in user data (Buildkite agent install + queue registration)

### 2.1 Prerequisites

1. Store your Buildkite agent token in SSM Parameter Store (SecureString):

```bash
aws ssm put-parameter \
  --name /buildkite/agent-token \
  --type SecureString \
  --value '<BUILDKITE_AGENT_TOKEN>' \
  --overwrite
```

2. Store a Tailscale auth key in SSM Parameter Store (SecureString):

```bash
aws ssm put-parameter \
  --name /tailscale/authkey/ci \
  --type SecureString \
  --value 'tskey-xxxxxxxxxxxxxxxx' \
  --overwrite
```

3. Pick a pinned Linux AMI ID in your region.
4. (Recommended) Store a read-only git deploy key in SSM:

```bash
aws ssm put-parameter \
  --name /buildkite/cleanroom/deploy-key \
  --type SecureString \
  --value '<OPENSSH_PRIVATE_KEY>' \
  --overwrite
```

### 2.2 Deploy (Linux-first recommended)

```bash
aws cloudformation deploy \
  --template-file infra/cloudformation/ci-hosts.yaml \
  --stack-name cleanroom-ci \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides \
    AvailabilityZone=ap-southeast-2a \
    LinuxAmiId=ami-0123456789abcdef0 \
    LinuxAsgRollingPauseTime=PT2M \
    BuildkiteTokenParameterName=/buildkite/agent-token \
    GitDeployKeyParameterName=/buildkite/cleanroom/deploy-key \
    TailscaleAuthKeyParameterName=/tailscale/authkey/ci \
    LinuxTailscaleAdvertiseTags='' \
    LinuxHostedAgentExtraTags=env=ci,team=platform \
    LinuxHostedAgentExtraConfig='priority=5;debug=true' \
    LinuxCleanroomAgentExtraTags=env=ci,team=platform,role=firecracker \
    CleanroomHelperGitRepositoryUrl=git@github.com:buildkite/cleanroom.git \
    CleanroomHelperGitRef=main
```

Notes:

- Default Linux instance type is `m8i.large` (nested virtualization) for the `cleanroom` queue.
- `LinuxAsgRollingPauseTime` controls how long CloudFormation waits after Linux instance replacement.
- `GitDeployKeyParameterName` installs a host-level `pre-checkout` hook that exports `GIT_SSH_COMMAND` for clone/fetch.
- TODO(cleanroom-ci): when CloudFormation supports `LaunchTemplateData.CpuOptions.NestedVirtualization`, move nested virtualization enablement out of the post-deploy EC2/ASG workaround and back into `infra/cloudformation/ci-hosts.yaml`.
- If you do not run Firecracker E2E, set:
  - `EnableCleanroomQueue=false`
  - `InstallFirecrackerOnLinux=false`
  - a smaller Linux instance type (for example `c7i.large`)

### 2.3 Tailscale SSH access

When `TailscaleAuthKeyParameterName` is set, bootstrap runs `tailscale up` on Linux with:

- unique hostnames based on instance ID (`<prefix>-<instance-id>`)
- `--ssh` enabled by default (set `TailscaleEnableSsh=false` to disable)
- optional `--advertise-tags` via `LinuxTailscaleAdvertiseTags`

Useful parameters:

- `LinuxTailscaleHostnamePrefix` (default `cleanroom-ci-linux`)
- `TailscaleAcceptRoutes` (default `false`)
- `LinuxTailscaleAdvertiseTags` (Linux only)

The stack outputs `LinuxTailscaleSshPattern` when Tailscale is enabled.
For Linux, replace `<instance-id>` with the active ASG instance ID.

### 2.4 Buildkite agent tags and config

The stack writes Buildkite config files on both hosts and supports extra per-agent tags/config:

- `LinuxHostedAgentExtraTags`
- `LinuxCleanroomAgentExtraTags`
- `LinuxHostedAgentExtraConfig`
- `LinuxCleanroomAgentExtraConfig`

`*ExtraConfig` values are semicolon-separated lines appended to agent config files.
Example:

```text
priority=5;debug=true;no-command-eval=true
```

## 3. Hosted Queues

No extra host setup is needed if you use `infra/cloudformation/ci-hosts.yaml`.

Notes:

- `mise` is bootstrapped via repository hooks in `.buildkite/hooks/`.
- Per-step cache is enabled for `hosted` and `mac-small` steps.
- Avoid global pipeline `cache:` blocks if self-hosted queues are present.

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

### 4.3 Privileged command execution modes

Firecracker backend supports two modes:

- `sudo` (default): direct `sudo -n <command>` execution
- `helper`: call a root-owned helper binary instead of direct sudo command execution

Runtime config keys:

- `backends.firecracker.privileged_mode`
- `backends.firecracker.privileged_helper_path`

For CI script usage, you can also set:

- `CLEANROOM_PRIVILEGED_MODE`
- `CLEANROOM_PRIVILEGED_HELPER_PATH`

#### Option A: default `sudo` mode

`sudo` mode requires NOPASSWD for commands used by launched execution:

```sudoers
User_Alias CLEANROOM_CI = buildkite-agent
Cmnd_Alias CLEANROOM_DOCTOR = /usr/bin/true, /usr/sbin/ip link show
Cmnd_Alias CLEANROOM_NET = /usr/sbin/ip *, /usr/sbin/iptables *, /usr/sbin/sysctl -w net.ipv4.ip_forward=1
Cmnd_Alias CLEANROOM_ROOTFS = /usr/bin/mount *, /usr/bin/umount *, /usr/bin/mkdir *, /usr/bin/install *

CLEANROOM_CI ALL=(root) NOPASSWD: CLEANROOM_DOCTOR, CLEANROOM_NET, CLEANROOM_ROOTFS
```

#### Option B: hardened `helper` mode (recommended)

Use a single root-owned helper binary and only grant sudo access to that helper:

Install helper from this repository:

```bash
sudo install -o root -g root -m 0755 scripts/cleanroom-root-helper.sh /usr/local/sbin/cleanroom-root-helper
```

```sudoers
buildkite-agent ALL=(root) NOPASSWD: /usr/local/sbin/cleanroom-root-helper *
```

Then set:

- `CLEANROOM_PRIVILEGED_MODE=helper`
- `CLEANROOM_PRIVILEGED_HELPER_PATH=/usr/local/sbin/cleanroom-root-helper`

When using `infra/cloudformation/ci-hosts.yaml` with `EnableCleanroomQueue=true`, the Linux bootstrap:

- starts a dedicated Buildkite agent process for the cleanroom queue
- configures `helper` mode via agent environment
- installs `/usr/local/sbin/refresh-cleanroom-helper` and downloads `/usr/local/sbin/cleanroom-root-helper` from git at host boot
- defaults to `CleanroomHelperGitRepositoryUrl=git@github.com:buildkite/cleanroom.git` and `CleanroomHelperGitRef=main`
- uses sudoers allowlist for `/usr/bin/install ... /usr/local/sbin/cleanroom-root-helper`

You can refresh helper on demand with SSM:

```bash
aws ssm send-command \
  --document-name AWS-RunShellScript \
  --targets "Key=tag:Name,Values=cleanroom-ci-linux" \
  --parameters commands='["sudo /usr/local/sbin/refresh-cleanroom-helper"]'
```

## 5. Optional Agent Environment Hook

If you prefer host-level env over pipeline step env, set variables in `/etc/buildkite-agent/hooks/environment`.

```bash
#!/usr/bin/env bash
set -euo pipefail

export CLEANROOM_KERNEL_IMAGE="/var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin"
export CLEANROOM_FIRECRACKER_BINARY="/usr/local/bin/firecracker"
```

## 6. Collision Safety

`scripts/ci-cleanroom-e2e.sh` isolates CI runtime paths using temporary XDG directories (`XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_RUNTIME_DIR`, `XDG_DATA_HOME`) and a job-local unix socket.

This prevents collisions with any long-running cleanroom instance on the same host.

## 7. Verification

After setup:

1. Trigger a build.
2. Confirm `:test_tube: Test (Linux)` and `:test_tube: Test (macOS)` pass.
3. Confirm `:fire: E2E (Firecracker)` passes doctor, launch, exec, and observability checks.

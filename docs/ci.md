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

## 2. Hosted and macOS Queues

No special setup is required beyond a working Buildkite agent image and internet access.

Notes:

- `mise` is bootstrapped via repository hooks in `.buildkite/hooks/`.
- Per-step cache is enabled for `hosted` and `mac-small` steps.
- Avoid global pipeline `cache:` blocks if self-hosted queues are present.

## 3. Cleanroom Queue (Firecracker E2E)

The `:fire: E2E (Firecracker)` step runs a real launched Firecracker execution and needs host preparation.

### 3.1 Required host capabilities

- Linux host with `/dev/kvm` available
- Firecracker binary (default `/usr/local/bin/firecracker`)
- Readable kernel and rootfs images for the `buildkite-agent` user
- Passwordless sudo for required network setup commands

### 3.2 Place runtime images

Put image assets under the Buildkite agent home so CI can read them:

```bash
sudo install -d -o buildkite-agent -g buildkite-agent /var/lib/buildkite-agent/.local/share/cleanroom/images
sudo cp /path/to/vmlinux.bin /var/lib/buildkite-agent/.local/share/cleanroom/images/
sudo cp /path/to/rootfs.ext4 /var/lib/buildkite-agent/.local/share/cleanroom/images/
sudo chown buildkite-agent:buildkite-agent /var/lib/buildkite-agent/.local/share/cleanroom/images/*
```

The pipeline is currently configured to use:

- `CLEANROOM_KERNEL_IMAGE=/var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin`
- `CLEANROOM_ROOTFS=/var/lib/buildkite-agent/.local/share/cleanroom/images/rootfs.ext4`
- `CLEANROOM_FIRECRACKER_BINARY=/usr/local/bin/firecracker`

### 3.3 Sudoers for full e2e

Create a constrained sudoers entry for networking commands used by the Firecracker backend:

```sudoers
User_Alias CLEANROOM_CI = buildkite-agent
Cmnd_Alias CLEANROOM_DOCTOR = /usr/bin/true, /usr/sbin/ip link show
Cmnd_Alias CLEANROOM_NET = /usr/sbin/ip *, /usr/sbin/iptables *, /usr/sbin/sysctl -w net.ipv4.ip_forward=1
Cmnd_Alias CLEANROOM_BATCH = /bin/sh -c *, /usr/bin/sh -c *

CLEANROOM_CI ALL=(root) NOPASSWD: CLEANROOM_DOCTOR, CLEANROOM_NET, CLEANROOM_BATCH
```

Install and validate:

```bash
sudo tee /etc/sudoers.d/buildkite-agent-cleanroom >/dev/null <<'EOF'
User_Alias CLEANROOM_CI = buildkite-agent
Cmnd_Alias CLEANROOM_DOCTOR = /usr/bin/true, /usr/sbin/ip link show
Cmnd_Alias CLEANROOM_NET = /usr/sbin/ip *, /usr/sbin/iptables *, /usr/sbin/sysctl -w net.ipv4.ip_forward=1
Cmnd_Alias CLEANROOM_BATCH = /bin/sh -c *, /usr/bin/sh -c *

CLEANROOM_CI ALL=(root) NOPASSWD: CLEANROOM_DOCTOR, CLEANROOM_NET, CLEANROOM_BATCH
EOF
sudo chmod 440 /etc/sudoers.d/buildkite-agent-cleanroom
sudo visudo -cf /etc/sudoers.d/buildkite-agent-cleanroom
```

## 4. Optional Agent Environment Hook

If you prefer host-level env over pipeline step env, set variables in `/etc/buildkite-agent/hooks/environment`.

```bash
#!/usr/bin/env bash
set -euo pipefail

export CLEANROOM_KERNEL_IMAGE="/var/lib/buildkite-agent/.local/share/cleanroom/images/vmlinux.bin"
export CLEANROOM_ROOTFS="/var/lib/buildkite-agent/.local/share/cleanroom/images/rootfs.ext4"
export CLEANROOM_FIRECRACKER_BINARY="/usr/local/bin/firecracker"
```

## 5. Collision Safety

`scripts/ci-cleanroom-e2e.sh` isolates CI runtime paths using temporary XDG directories (`XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_RUNTIME_DIR`, `XDG_DATA_HOME`) and a job-local unix socket.

This prevents collisions with any long-running cleanroom instance on the same host.

## 6. Verification

After setup:

1. Trigger a build.
2. Confirm `:test_tube: Test (Linux)` and `:test_tube: Test (macOS)` pass.
3. Confirm `:fire: E2E (Firecracker)` passes doctor, launch, exec, and observability checks.

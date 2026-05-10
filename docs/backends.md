# Backends

Cleanroom runs Linux microVMs on both Linux and macOS. The public CLI and policy
surface is the same; backend-specific details live in runtime config and
adapter internals.

| Host OS | Backend | Notes |
|---------|---------|-------|
| Linux | `firecracker` | KVM-backed microVMs, per-sandbox TAP devices, host firewall egress enforcement, file-backed or ZFS-backed snapshots |
| macOS | `darwin-vz` | Apple Virtualization.framework microVMs, filehandle networking, TCP egress filtering, APFS snapshots when supported |

## Choosing A Backend

The default backend is chosen for the host OS. Override it in runtime config:

```yaml
default_backend: darwin-vz
```

Or for one command:

```bash
cleanroom exec --backend firecracker -- uname -a
```

## Linux

The Linux backend uses Firecracker and KVM. Host setup needs:

- Firecracker binary
- Linux kernel image and rootfs configured in runtime config
- helper support for host networking and snapshot operations
- iptables support for egress enforcement

`firecracker` supports deny-by-default allowlist egress. Stage-scoped network
policies currently fail closed on this backend.

See [Firecracker Backend](backend/firecracker.md) for details.

## macOS

The macOS backend uses a signed Swift helper built on
Virtualization.framework. Host setup needs:

- `cleanroom-darwin-vz` helper
- kernel and rootfs assets
- `mkfs.ext4` and `debugfs` from `e2fsprogs`

`darwin-vz` supports filehandle TCP egress filtering and stage-scoped network
policies. It does not expose Firecracker-style TAP devices or host-visible
guest IP identity.

See [Darwin VZ Backend](backend/darwin-vz.md) for details.

## Capabilities

Run:

```bash
cleanroom doctor
cleanroom doctor --json
```

Doctor reports backend availability, required host tools, snapshot support, and
capability gaps for the current runtime config.

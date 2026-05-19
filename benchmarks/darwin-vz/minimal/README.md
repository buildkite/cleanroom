# Minimal darwin-vz Benchmark

This directory contains a deliberately small Virtualization.framework benchmark for darwin-vz boot experiments. It is a lower-bound probe, not a Cleanroom backend and not a replacement for `scripts/benchmark-tti.sh`.

The runner boots a Linux kernel through `VZLinuxBootLoader`, connects to a guest vsock listener, runs a probe, and prints JSON timings for VM start, vsock readiness, and first exec response. The default `exec` probe runs `/bin/true`; the `memory-reporting` probe runs `/bin/memprobe`, touches guest memory, frees it, and records host footprint samples so VZ balloon and free-page-reporting behavior can be tested.

## Layout

- `runner.swift`: standalone Virtualization.framework runner.
- `initrd-agent`: tiny Linux `/init` that implements enough of the guest exec protocol for `/bin/true`.
- `initrd-true`: static no-op command used by the initrd benchmark.
- `initrd-memprobe`: static guest memory lifecycle probe used by the `memory-reporting` benchmark.
- `build-runner.sh`: builds and signs `dist/darwin-vz-minimal`.
- `build-initrd.sh`: builds `dist/darwin-vz-minimal-initrd.cpio.gz`.
- `build-kernel.sh`: Docker-based minimal Linux kernel builder for initrd or rootfs profiles. The initrd kernel enables virtio balloon and Linux page reporting support for memory experiments.

## Run

To run with an existing kernel:

```bash
scripts/benchmark-darwin-vz-minimal.sh \
  --kernel dist/darwin-vz-minimal-initrd-arm64-kernel-6.1.155-Image \
  --iterations 30
```

To build the minimal kernel first:

```bash
scripts/benchmark-darwin-vz-minimal.sh --build-kernel --iterations 30
```

To build the rootfs-profile kernel for the Cleanroom `darwin-vz` backend:

```bash
mise run build:kernel:darwin-vz-minimal-rootfs
```

Then set `backends.darwin-vz.kernel_image` to the generated
`dist/darwin-vz-minimal-rootfs-arm64-kernel-6.1.155-Image` path.

To test whether VZ releases host footprint after guest free-page reporting without an explicit balloon target:

```bash
scripts/benchmark-darwin-vz-minimal.sh \
  --build-kernel \
  --iterations 3 \
  --memory-mib 8192 \
  --probe memory-reporting \
  --probe-memory-mib 4096
```

Useful controls:

```bash
# Negative control: no VZ balloon device is attached.
scripts/benchmark-darwin-vz-minimal.sh \
  --build-kernel \
  --iterations 1 \
  --memory-mib 8192 \
  --probe memory-reporting \
  --probe-memory-mib 4096 \
  --balloon-device off

# Positive control: explicitly request VZ to reclaim memory after the guest frees it.
scripts/benchmark-darwin-vz-minimal.sh \
  --build-kernel \
  --iterations 1 \
  --memory-mib 8192 \
  --probe memory-reporting \
  --probe-memory-mib 4096 \
  --balloon-target-mib 1024
```

To test an early low target with later growth before the workload:

```bash
scripts/benchmark-darwin-vz-minimal.sh \
  --build-kernel \
  --iterations 1 \
  --memory-mib 8192 \
  --initial-balloon-target-mib 1024 \
  --pre-probe-balloon-target-mib 8192 \
  --pre-probe-balloon-settle-ms 1000 \
  --probe memory-reporting \
  --probe-memory-mib 4096
```

The wrapper writes JSONL results to `benchmarks/results/` and leaves build outputs in `dist/`.

Use this benchmark when testing kernel, initrd, VZ device, or guest-readiness experiments. Use `scripts/benchmark-tti.sh` when measuring end-to-end Cleanroom user-facing startup.

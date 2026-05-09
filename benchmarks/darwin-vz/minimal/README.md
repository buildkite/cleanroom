# Minimal darwin-vz Benchmark

This directory contains a deliberately small Virtualization.framework benchmark for darwin-vz boot experiments. It is a lower-bound probe, not a Cleanroom backend and not a replacement for `scripts/benchmark-tti.sh`.

The runner boots a Linux kernel through `VZLinuxBootLoader`, connects to a guest vsock listener, runs `/bin/true`, and prints JSON timings for VM start, vsock readiness, and first exec response.

## Layout

- `runner.swift`: standalone Virtualization.framework runner.
- `initrd-agent`: tiny Linux `/init` that implements enough of the guest exec protocol for `/bin/true`.
- `initrd-true`: static no-op command used by the initrd benchmark.
- `build-runner.sh`: builds and signs `dist/darwin-vz-minimal`.
- `build-initrd.sh`: builds `dist/darwin-vz-minimal-initrd.cpio.gz`.
- `build-kernel.sh`: Docker-based minimal Linux kernel builder for initrd or rootfs profiles.

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

The wrapper writes JSONL results to `benchmarks/results/` and leaves build outputs in `dist/`.

Use this benchmark when testing kernel, initrd, VZ device, or guest-readiness experiments. Use `scripts/benchmark-tti.sh` when measuring end-to-end Cleanroom user-facing startup.

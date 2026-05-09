# Agent Notes

- Keep implementation backend-agnostic by default.
- Prefer backend-neutral CLI/API surfaces; place backend-specific runtime details in runtime config (XDG config) and adapter internals.
- For darwin-vz boot-time experiments, prefer the minimal benchmark harness under `benchmarks/darwin-vz/minimal` before changing the production adapter path.
- This project is in early development: breaking changes are acceptable, and legacy/backwards-compat paths are not required unless explicitly requested.

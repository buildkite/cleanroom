# Agent Notes

- Keep implementation backend-agnostic by default.
- Prefer backend-neutral CLI/API surfaces; place backend-specific runtime details in runtime config (XDG config) and adapter internals.
- This project is in early development: breaking changes are acceptable, and legacy/backwards-compat paths are not required unless explicitly requested.

# Agent Notes

- Cleanroom is a policy compiler and provenance layer over the external
  `spore` CLI (sporevm); it does not run VMs itself. Keep the surface small:
  `policy validate`, `compile`, `stamp`, `bake`, `verify`, `gateway serve`,
  `version`.
- This project is in early development: breaking changes are acceptable, and
  legacy/backwards-compat paths are not required unless explicitly requested.

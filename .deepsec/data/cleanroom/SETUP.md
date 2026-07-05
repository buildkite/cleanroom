# Agent setup for `cleanroom`

This is a deepsec scanning workspace for project `cleanroom` (target:
`..`). The current `data/cleanroom/INFO.md` is already filled for the
SporeVM and bake surface and should be treated as the active project
context for scans.

## Maintenance guidance

- Keep `data/cleanroom/INFO.md` short and selective: it is injected into
  every AI review prompt, so include only context a reviewer would miss
  without repo-specific knowledge.
- Prefer 3–5 representative items per section rather than exhaustive file,
  helper, or callsite inventories.
- Name project-specific primitives and behaviours, but avoid generic CWE
  explanations that built-in matchers already cover.
- Update INFO.md when the CLI surface, policy compiler, bake staging,
  provenance, mediation gateway, installer, or release workflows change in
  a security-relevant way.

## Source material for future updates

Read enough of the repo to keep INFO.md accurate, using these sources as
starting points:

- `../README.md`
- `../docs/policy.md`
- `../docs/plans/sporevm-layer.md`
- `../go.mod`
- Representative files in `../internal/bake`
- Representative files in `../internal/policy`
- Representative files in `../internal/mediation`
- Representative files in `../internal/cli`
- Representative scripts in `../scripts`

Focus on Cleanroom's actual trust boundaries: policy compilation that
fails closed, git-visible workspace staging for bake, provenance and bake
key validation, host-side mediation gateway grants and credential
replacement, and archive checksum verification.

## Custom matchers

Only add custom matchers after a confirmed true positive demonstrates a
repo-specific pattern worth preserving. Before writing one, read
`node_modules/deepsec/dist/docs/writing-matchers.md` and build the matcher
from the confirmed finding rather than speculating.

## Running scans

Typical commands from `.deepsec/` are:

```bash
pnpm deepsec scan    --project-id cleanroom
pnpm deepsec process --project-id cleanroom
```

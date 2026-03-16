# Inference credential mediation plan

This document captures a Cleanroom-aligned design direction for allowing
inference and agent-tool calls from sandboxes without injecting long-lived
credentials into guest environment variables or guest-visible files.

## Goals

- Keep inference credentials on the host side of the sandbox boundary.
- Reuse Cleanroom's existing mediation model rather than creating a second
  credential path outside the gateway.
- Keep repository policy backend-agnostic and runtime credential resolution
  host-local.
- Support both generic inference APIs and agent-specific CLIs where practical.

## Non-goals

- Making every upstream provider look identical when their auth and protocol
  surfaces differ materially.
- Supporting arbitrary guest-configured HTTP proxying.
- Treating mounted auth files or long-lived env vars as acceptable steady-state
  solutions.

## Proposal A: inference gateway as the target architecture

Extend the existing host gateway with explicit inference and agent routes, for
example:

- `/llm/openai/v1/...`
- `/llm/anthropic/v1/...`
- `/agent/amp/...`

Properties:

- sandbox identity continues to come from transport identity:
  - Firecracker: source IP on TAP network
  - `darwin-vz`: scoped capability token fallback
- credentials stay entirely host-side and are resolved by provider adapters
- the guest receives only non-secret routing/config such as endpoint URLs,
  helper command paths, or generated config files
- audit events stay centralized in the gateway

Repository policy should declare capability, not credential location:

```yaml
inference:
  allow:
    - service: openai
      purpose: codex
      models: [gpt-5-codex]
      binding: codex_account
    - service: anthropic
      purpose: claude-code
      models: [claude-sonnet-4-5]
      binding: claude_code
    - service: amp
      purpose: amp-cli
      binding: amp_account
```

Runtime config should define how bindings are resolved on the host:

```toml
[credential_bindings.codex_account]
kind = "codex-auth-state"
path = "~/.codex/auth.json"

[credential_bindings.claude_code]
kind = "command"
command = ["cleanroom-credential-helper", "claude"]

[credential_bindings.amp_account]
kind = "amp-secret-store"
path = "~/.local/share/amp/secrets.json"
```

Rationale:

- matches Cleanroom's gateway-first architecture
- keeps provider-specific auth details out of repo policy
- preserves a single audit and enforcement point
- works for both direct inference APIs and mediated agent services

## Proposal B: helper bridge as a narrow early slice

For tools that already support credential-helper hooks, add a small broker path
under `/secrets/` and configure the tool to call it through a guest-side stub.

Best fit:

- Claude Code, because `apiKeyHelper` is already part of the documented surface

Tradeoffs:

- smaller and faster to ship than a full request mediation gateway
- still returns a real provider credential to the guest process, even if
  short-lived
- does not map cleanly to Codex or Amp's currently observed auth surfaces

This is useful as a bootstrap path, but it should not replace host-side request
mediation as the long-term architecture.

## Proposal C: host-executed agent adapters

For tools whose auth model is strongly tied to host login state, run the agent
on the host and expose it to the guest as a mediated Cleanroom tool surface.

Properties:

- the guest invokes a Cleanroom-owned command or RPC surface
- the actual provider CLI runs on the host with host credentials
- stdout/stderr and tool/file access are streamed or mediated explicitly

Best fit:

- Codex and Amp, if their auth stores remain awkward to proxy cleanly from
  inside the guest

Tradeoffs:

- strongest credential isolation
- less transparent than running the provider CLI natively inside the sandbox
- more opinionated execution model

## Recommended order

1. Treat Proposal A as the target architecture.
2. Use Proposal B selectively for Claude as an early proving ground.
3. Use Proposal C for provider CLIs that remain strongly host-login-shaped.

## Design rule

- The guest may know where to send an inference request.
- The host decides whether to forward it.
- The host owns the credential.
- Repository policy decides whether the capability exists at all.

# Network Allow Syntax Plan

**Spec reference:** `docs/spec.md` section 5.2
**Status:** Implemented
**Last reviewed:** 2026-04-27

## Summary

Accept terse allow entries in the existing `sandbox.network.allow` policy block
without changing compiled policy, proto payloads, cache keys, gateway behavior,
or backend enforcement.

This is a parser and documentation change only. The canonical model remains
structured host-plus-ports allow rules.

## Problem

The current allowlist syntax is explicit but verbose:

```yaml
sandbox:
  network:
    default: deny
    allow:
      - host: github.com
        ports: [443]
      - host: proxy.golang.org
        ports: [443]
      - host: registry.npmjs.org
        ports: [443, 80]
```

Most rules allow a single HTTPS destination. Requiring map syntax for every
single-port host makes common policy files noisier than necessary.

## Policy Shape

Support string entries in `sandbox.network.allow`:

```yaml
sandbox:
  network:
    default: deny
    allow:
      - github.com:443
      - proxy.golang.org:443
      - host: registry.npmjs.org
        ports: [443, 80]
```

Also accept a single string as a singleton allowlist:

```yaml
sandbox:
  network:
    default: deny
    allow: github.com:443
```

## Semantics

- String form is `host:port` only.
- Bare hosts such as `github.com` are rejected.
- URLs such as `https://github.com` are rejected.
- Ports must be explicit and valid.
- Map form remains `host` plus `ports`.
- Do not make `host: github.com:443` special in map form.
- IPv6 shorthand is reserved for later; reject it initially unless bracketed
  literal support is deliberately added.
- All accepted forms normalize to the existing `AllowRule{Host, Ports}` model.

## Implementation Plan

1. Replace the raw allow-rule representation with a YAML unmarshaller that
   accepts either a scalar string or a mapping.
2. Parse scalar strings with `net.SplitHostPort` or equivalent strict logic so
   host and port boundaries are unambiguous.
3. Reuse the current host normalization, port validation, duplicate removal, and
   sorting behavior after parsing.
4. Keep `CompiledPolicy.Allow`, `PolicyAllowRule`, client types, and generated
   proto output unchanged.
5. Update policy tests for scalar list entries, singleton scalar allowlists,
   mixed scalar/map entries, and invalid shorthand.
6. Update `docs/spec.md` and examples to show the shorthand for single-port
   hosts while retaining map examples for multiple ports.

## Definition Of Done

- `cleanroom policy validate` accepts scalar and map allow entries.
- Existing structured allow entries still work.
- Invalid shorthand fails closed with field-specific errors.
- Compiled policy output for shorthand input is identical to equivalent map
  input.
- No backend, gateway, proto, or cache behavior changes.

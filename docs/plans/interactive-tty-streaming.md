# Batch/Interactive Execution Split Plan (QUIC-First)

**Status:** proposed
**Scope:** control API, controlservice, backends, CLI execution flows

## Summary

Redesign execution around two explicit modes:

1. `batch`: durable, log-oriented, replayable command execution.
2. `interactive`: low-latency PTY streaming for TUIs and long-running agents.

Keep Connect RPC for control-plane operations and move interactive bytes to a dedicated QUIC data plane implemented with `github.com/quic-go/quic-go`.

## PR sequencing

1. PR 1 (prep): remove built-in Tailscale listener modes (`tsnet://`, `tssvc://`) and remove mTLS management/auth paths; keep plain HTTPS + server-auth TLS.
2. PR 2: add explicit execution kinds + `OpenInteractiveExecution` bootstrap RPC.
3. PR 3: add server-side QUIC interactive transport and session registry.
4. PR 4: migrate CLI interactive commands (`console`, `agent codex`) to QUIC path and delete legacy attach stream.
5. PR 5: hardening, soak tests, qlog/metrics, docs cleanup.

## Why change

Current interactive behavior traverses layers built for persistence:

1. guest JSON/base64 frames
2. backend decode + restream
3. controlservice event history + subscriber queues
4. attach frame translation to CLI

This adds copies, queue pressure, and burst fragility. TUI redraw workloads are sensitive to this path shape.

## Design principles

1. Separate control plane from interactive data plane.
2. Preserve PTY byte semantics in interactive mode.
3. Keep durability and replay in batch mode.
4. Stay backend-neutral at CLI/API boundaries.
5. Optimize for remote backends as a first-class case.
6. Do not implement TCP/WebSocket fallback for UDP-blocked networks in v1.

## Prior art and concrete libraries

Behavioral prior art:

1. SSH session/channel model (`shell` PTY stream + `window-change` + `signal` + `exit-status`).
2. Kubernetes exec stream split semantics.

Concrete Go stack:

1. `github.com/quic-go/quic-go` for interactive network transport.
2. `github.com/creack/pty` + `golang.org/x/term` for local terminal handling.
3. `connectrpc` remains control-plane RPC transport.

Why `quic-go` here:

1. multiplexed streams with per-stream flow control reduce PTY/control interference.
2. no TCP head-of-line blocking for lossy remote links.
3. built-in keepalive/idle timeout controls for long-running agents.
4. built-in qlog tracing for low-level debugging.

## Target architecture

## 1) Execution kinds are explicit

Replace implicit `tty` behavior with explicit execution kind:

1. `EXECUTION_KIND_BATCH`
2. `EXECUTION_KIND_INTERACTIVE`

`batch` and `interactive` become product semantics, not transport side-effects.

## 2) Batch mode (durable path)

Batch characteristics:

1. stdout/stderr events are persisted and replayable.
2. stream consumers may reconnect and catch up via history.
3. output can be bounded/truncated with predictable retention policy.
4. ideal for `cleanroom exec` and CI-like tasks.

## 3) Interactive mode (real-time QUIC path)

Interactive characteristics:

1. one active PTY byte stream (terminal semantics).
2. low-latency bidirectional channel for `stdin`, `resize`, `signal`, `close`.
3. no event-history fanout in hot output path.
4. minimal persisted state only (`running`, `exit`, timestamps, optional tiny tail).
5. ideal for `cleanroom console` and `cleanroom agent codex`.

## 4) QUIC data plane v1

### 4.1 Session bootstrap (control plane)

Use Connect RPC to open interactive sessions, then pivot to QUIC:

1. `CreateExecution(kind=INTERACTIVE, ...)`.
2. `OpenInteractiveExecution(execution_id, sandbox_id, initial_tty)` returns:
   - `session_id`
   - `quic_endpoint` (host:port)
   - `alpn` (`cleanroom-interactive-v1`)
   - `session_token` (single-use, short TTL)
   - `server_cert_pin_sha256` (for pin validation when needed)
   - `expires_at`

No PTY payload bytes flow through Connect after bootstrap.

### 4.2 QUIC stream layout (minimal)

Per interactive session, client opens one QUIC connection and three streams:

1. bidirectional control stream (protobuf messages, length-delimited):
   - client -> server: `hello`, `resize`, `signal`, `stdin_eof`, `detach`
   - server -> client: `hello_ack`, `exit`, `error`
2. unidirectional `stdin` stream (client -> server raw bytes).
3. unidirectional `pty` stream (server -> client raw PTY bytes).

No JSON, no base64, no per-frame event envelopes in the hot byte path.

### 4.3 Authentication and ownership

1. server validates `session_token` in control `hello` before accepting stream traffic.
2. token is single-use and expires quickly (for example, 30 seconds).
3. exactly one writer attach per execution session.
4. when attach drops, execution continues unless explicit `detach=false` close is sent.

### 4.4 TLS and endpoint strategy

1. ALPN: `cleanroom-interactive-v1`.
2. for remote HTTPS deployments: reuse configured server-auth TLS trust chain.
3. for local unix/http deployments: use certificate pin returned by `OpenInteractiveExecution`.
4. client verifies either PKI trust or pin before sending token.

### 4.5 `quic-go` defaults for v1

Start conservative and tune with data:

1. `HandshakeIdleTimeout`: `10s`.
2. `MaxIdleTimeout`: `45s`.
3. `KeepAlivePeriod`: `15s`.
4. `InitialStreamReceiveWindow`: `256 KiB`.
5. `MaxStreamReceiveWindow`: `2 MiB`.
6. `InitialConnectionReceiveWindow`: `1 MiB`.
7. `MaxConnectionReceiveWindow`: `8 MiB`.
8. `MaxIncomingStreams`: `8`.
9. `MaxIncomingUniStreams`: `8`.

Transport shape:

1. server owns one UDP socket and one `quic.Transport`.
2. accept loop dispatches each QUIC connection to an interactive session handler.
3. client can use `DialAddr` initially; move to shared `quic.Transport` if needed later.

### 4.6 Close/error semantics

1. normal completion:
   - server sends control `exit(code,status)`.
   - server closes `pty` stream and QUIC connection with app code `0`.
2. auth failure:
   - close with app code `1001` (`unauthorized`).
3. protocol violation:
   - close with app code `1002` (`protocol_error`).
4. execution canceled/terminated:
   - send `exit` with status + code, then close.

Client maps these to stable CLI errors.

## API changes (breaking, intentional)

1. `CreateExecutionRequest` takes `kind` instead of inferring from `tty`.
2. add unary `OpenInteractiveExecution`.
3. `StreamExecution` remains batch-only durable stream.
4. deprecate `AttachExecution` once `console` and `agent` migrate.

CLI/API UX expectation:

1. users do not choose modes manually in normal flow.
2. command intent selects mode:
   - `exec` -> batch
   - `console` / `agent codex` -> interactive
3. optional hidden override can exist for troubleshooting.

## Backend contract changes

Replace one-size-fits-all run path with mode-specific adapter contracts:

1. `RunBatch(ctx, req, sink) -> result`
2. `StartInteractive(ctx, req) -> InteractiveSession`

`InteractiveSession` interface:

1. `ReadPTY([]byte) (int, error)`
2. `WriteStdin([]byte) error`
3. `Resize(cols, rows uint32) error`
4. `Signal(int32) error`
5. `Close() error`
6. `Wait() (exitCode int32, err error)`

Adapters keep backend-specific runtime details internal.

## Minimal implementation plan

### Slice 0: API/schema split

1. add `ExecutionKind`.
2. add `OpenInteractiveExecution` RPC/messages.
3. keep old `tty` field only as temporary shim in the active migration branch.

Definition of done:

1. API can represent and bootstrap batch vs interactive explicitly.

### Slice 1: controlservice session registry

1. track interactive session metadata (`session_id`, token hash, owner, ttl, execution binding).
2. enforce single attach owner.
3. expose `OpenInteractiveExecution` from service layer.

Definition of done:

1. session tokens are minted, validated, and invalidated on first use.

### Slice 2: server QUIC accept path

1. add `internal/interactivequic` package.
2. initialize QUIC listener/transport in `internal/controlserver/server.go`.
3. accept QUIC connections, run hello/token validation, bind to `InteractiveSession`.
4. bridge `stdin`/`pty`/control streams between QUIC and backend adapter.

Definition of done:

1. PTY bytes do not pass through execution event subscriber queues.

### Slice 3: client QUIC attach path

1. add client-side dialer in `internal/controlclient/client.go`.
2. in `internal/cli/cli.go` console/agent flows:
   - call `OpenInteractiveExecution`
   - dial QUIC
   - map local raw terminal to QUIC streams
3. keep batch path unchanged.

Definition of done:

1. `cleanroom console` and `cleanroom agent codex` use QUIC interactive path end-to-end.

### Slice 4: remove legacy interactive stream

1. delete `AttachExecution` internals and proto fields once cutover is complete.
2. keep only batch stream semantics in `StreamExecution`.
3. update docs (`docs/api.md`, CLI help).

Definition of done:

1. no interactive user path depends on Connect bidi attach frames.

### Slice 5: observability and hardening

1. wire qlog tracer (`Config.Tracer`) behind debug flag/env.
2. expose counters: bytes in/out, resize count, signal count, disconnect reason, idle timeout.
3. soak tests with high ANSI redraw + resize bursts.

Definition of done:

1. no blank/frozen TUI under sustained redraw in soak tests.

## Test plan

1. Unit:
   - execution kind validation
   - session token mint/validate/invalidate
   - control stream message codec
2. Integration:
   - `exec` durability/replay unchanged
   - `console` resize/signal/exit correctness over QUIC
   - `agent codex --no-alt-screen` and alt-screen smoke tests
3. Soak:
   - 10-minute redraw workload with periodic resize and signal
   - assert session continuity and clean exit propagation
4. Network behavior:
   - packet loss and reorder simulation
   - verify no stuck blank screen and bounded reconnect errors

## Risks

1. UDP blocked networks will fail interactive mode (accepted non-goal for v1).
2. token + cert pin mistakes can cause hard-to-debug attach failures.
3. split planes increase operational moving parts.

## Open questions

1. Should we include optional read-only observers in v1 or defer?
2. Should we expose an explicit `cleanroom debug interactive` command to dump qlog paths?
3. Do we need optional compression for high-latency, low-bandwidth links after baseline measurements?

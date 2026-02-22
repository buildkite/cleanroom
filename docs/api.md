# Cleanroom Minimal API (ConnectRPC)

## 1) Goal

Define a minimal, functional API for creating and managing Cleanroom sandboxes with streaming execution support.

This document is intentionally small-scope:
- management plane for sandbox lifecycle
- execution plane for command runs and streaming output

## 2) Terminology

- `Sandbox`: A policy-bound execution environment created from an immutable `CompiledPolicy`.
- `Execution`: A single command invocation inside a sandbox.
- `Event`: Structured runtime and policy events emitted during sandbox/execution lifecycle.

`sandbox` is the top-level API noun. `run_id` remains an internal/audit identifier.

## 3) Transport Choice

Use ConnectRPC with HTTP/2 enabled in server deployments.

Why:
- unary RPCs for lifecycle operations
- server-streaming for event/log tails
- bidirectional stream for interactive exec (`stdin` -> guest, `stdout/stderr/events` <- guest)

Non-goal for v1:
- REST endpoint support.

## 3.1 Process Model (Canonical)

Cleanroom uses a client/server architecture for all user-facing operations.

1. `cleanroom` CLI is always a client.
2. The server process is authoritative for sandbox lifecycle and execution state.
3. "Local" behavior means "client talking to local server endpoint", not a separate direct execution path.

### Binary decision (v1)

Use a single binary in v1:
- primary executable: `cleanroom`
- server mode: `cleanroom serve`

This keeps one code path while still supporting systemd/launchd unit ergonomics via a single executable.

## 3.2 Endpoint Model

Default endpoint:
- `unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock`

Fallbacks:
1. `--host` flag
2. `CLEANROOM_HOST`
3. active context config
4. default unix socket path

HTTP and Tailscale endpoints are also supported:
- `http://host:port` or `https://host:port` for direct HTTP
- `tsnet://hostname[:port]` for embedded Tailscale (tsnet)
- `tssvc://service[:local-port]` for Tailscale Services

## 4) API Surface (Minimal v1)

Two services are sufficient.

1. `SandboxService`
2. `ExecutionService`

### 4.1 SandboxService

1. `CreateSandbox(CreateSandboxRequest) returns (CreateSandboxResponse)` (unary)
2. `GetSandbox(GetSandboxRequest) returns (GetSandboxResponse)` (unary)
3. `ListSandboxes(ListSandboxesRequest) returns (ListSandboxesResponse)` (unary)
4. `DownloadSandboxFile(DownloadSandboxFileRequest) returns (DownloadSandboxFileResponse)` (unary)
5. `TerminateSandbox(TerminateSandboxRequest) returns (TerminateSandboxResponse)` (unary)
6. `StreamSandboxEvents(StreamSandboxEventsRequest) returns (stream SandboxEvent)` (server-streaming)

### 4.2 ExecutionService

1. `CreateExecution(CreateExecutionRequest) returns (CreateExecutionResponse)` (unary)
2. `GetExecution(GetExecutionRequest) returns (GetExecutionResponse)` (unary)
3. `CancelExecution(CancelExecutionRequest) returns (CancelExecutionResponse)` (unary)
4. `StreamExecution(StreamExecutionRequest) returns (stream ExecutionStreamEvent)` (server-streaming)
5. `AttachExecution(stream ExecutionAttachFrame) returns (stream ExecutionAttachFrame)` (bidirectional)

`AttachExecution` is for interactive sessions and signaling (stdin, resize, heartbeat, close, stdout/stderr, exit).

## 5) Resource and State Model

### 5.1 Sandbox statuses

- `SANDBOX_STATUS_PROVISIONING`
- `SANDBOX_STATUS_READY`
- `SANDBOX_STATUS_STOPPING`
- `SANDBOX_STATUS_STOPPED`
- `SANDBOX_STATUS_FAILED`

### 5.2 Execution statuses

- `EXECUTION_STATUS_QUEUED`
- `EXECUTION_STATUS_RUNNING`
- `EXECUTION_STATUS_SUCCEEDED`
- `EXECUTION_STATUS_FAILED`
- `EXECUTION_STATUS_CANCELED`
- `EXECUTION_STATUS_TIMED_OUT`

## 6) Policy and Security Invariants

1. `CreateSandbox` compiles policy once and persists:
   - `compiled_policy`
   - `policy_hash`
   - `backend`
2. Active sandbox policy is immutable for sandbox lifetime.
3. Backend capability validation occurs before provisioning.
4. Launch fails closed on capability mismatch.
5. Secret values are never returned by API responses/streams.

These rules align with `SPEC.md` requirements for policy immutability and capability gating.

## 7) Error Contract

All RPC errors must include:
- stable machine code (`error.code`)
- human-readable message (`error.message`)
- optional details (`error.details`)

Minimum v1 codes:
- `policy_invalid`
- `policy_conflict`
- `backend_unavailable`
- `backend_capability_mismatch`
- `host_not_allowed`
- `registry_not_allowed`
- `lockfile_violation`
- `secret_scope_violation`
- `runtime_launch_failed`

For ConnectRPC, map these into typed error details and a stable application code field.

## 8) Minimal Proto Sketch

```proto
syntax = "proto3";

package cleanroom.v1;

import "google/protobuf/timestamp.proto";

service SandboxService {
  rpc CreateSandbox(CreateSandboxRequest) returns (CreateSandboxResponse);
  rpc GetSandbox(GetSandboxRequest) returns (GetSandboxResponse);
  rpc ListSandboxes(ListSandboxesRequest) returns (ListSandboxesResponse);
  rpc DownloadSandboxFile(DownloadSandboxFileRequest) returns (DownloadSandboxFileResponse);
  rpc TerminateSandbox(TerminateSandboxRequest) returns (TerminateSandboxResponse);
  rpc StreamSandboxEvents(StreamSandboxEventsRequest) returns (stream SandboxEvent);
}

service ExecutionService {
  rpc CreateExecution(CreateExecutionRequest) returns (CreateExecutionResponse);
  rpc GetExecution(GetExecutionRequest) returns (GetExecutionResponse);
  rpc CancelExecution(CancelExecutionRequest) returns (CancelExecutionResponse);
  rpc StreamExecution(StreamExecutionRequest) returns (stream ExecutionStreamEvent);
  rpc AttachExecution(stream ExecutionAttachFrame) returns (stream ExecutionAttachFrame);
}

message Sandbox {
  string sandbox_id = 1;
  SandboxStatus status = 2;
  string backend = 3;
  string policy_hash = 4;
  google.protobuf.Timestamp created_at = 5;
  google.protobuf.Timestamp updated_at = 6;
}

enum SandboxStatus {
  SANDBOX_STATUS_UNSPECIFIED = 0;
  SANDBOX_STATUS_PROVISIONING = 1;
  SANDBOX_STATUS_READY = 2;
  SANDBOX_STATUS_STOPPING = 3;
  SANDBOX_STATUS_STOPPED = 4;
  SANDBOX_STATUS_FAILED = 5;
}

message Execution {
  string execution_id = 1;
  string sandbox_id = 2;
  ExecutionStatus status = 3;
  repeated string command = 4;
  int32 exit_code = 5;
  google.protobuf.Timestamp started_at = 6;
  google.protobuf.Timestamp finished_at = 7;
  bool tty = 8;
  string run_id = 9;
}

enum ExecutionStatus {
  EXECUTION_STATUS_UNSPECIFIED = 0;
  EXECUTION_STATUS_QUEUED = 1;
  EXECUTION_STATUS_RUNNING = 2;
  EXECUTION_STATUS_SUCCEEDED = 3;
  EXECUTION_STATUS_FAILED = 4;
  EXECUTION_STATUS_CANCELED = 5;
  EXECUTION_STATUS_TIMED_OUT = 6;
}
```

## 9) CLI Subcommands

Expose a server mode and API-driven commands in the same binary.

### 9.1 Server

- `cleanroom serve --listen unix:///run/user/1000/cleanroom/cleanroom.sock`
- `cleanroom serve --listen tcp://127.0.0.1:7777`
- `cleanroom serve --listen tcp://0.0.0.0:7777 --tls-cert ... --tls-key ...`
- `cleanroom serve --listen tsnet://cleanroom:7777`
- `cleanroom serve --listen tssvc://cleanroom`

### 9.2 Sandbox commands

- `cleanroom --host unix:///run/user/1000/cleanroom/cleanroom.sock sandboxes create --backend local --policy ./cleanroom.yaml --repo .`
- `cleanroom sandboxes get <sandbox-id>`
- `cleanroom sandboxes list`
- `cleanroom sandboxes terminate <sandbox-id>`
- `cleanroom sandboxes events <sandbox-id> [--follow]`

### 9.3 Execution commands

- `cleanroom executions create <sandbox-id> -- "npm test"`
- `cleanroom executions get <sandbox-id> <execution-id>`
- `cleanroom executions cancel <sandbox-id> <execution-id>`
- `cleanroom executions stream <sandbox-id> <execution-id>`
- `cleanroom executions attach <sandbox-id> <execution-id>`

### 9.4 Local UX

`cleanroom exec` remains the primary developer UX, but it is implemented as client/server RPC.

Default command:
- `cleanroom exec -- "npm test"`

Additional command forms:
- `cleanroom exec -it -- bash`

Behavior contract:
1. Resolve server endpoint (`--host`, `CLEANROOM_HOST`, context, default unix socket).
2. Resolve and compile repository policy.
3. Create sandbox (default: ephemeral sandbox for this invocation).
4. Create execution with command and TTY options.
5. Stream output:
   - non-interactive: `StreamExecution`
   - interactive: `AttachExecution`
6. Return the command exit code.
7. If sandbox is ephemeral, terminate it after execution completion.

Signal behavior:
1. First `Ctrl-C`: `CancelExecution`.
2. Second `Ctrl-C`: force detach client stream and return immediately.

Failure UX:
- Always print `sandbox_id` and `execution_id` when available.
- On policy/runtime deny, print stable reason code and a follow-up command to inspect events.

## 10) Suggested Implementation Plan

1. Define protobufs and generate ConnectRPC stubs.
2. Implement `cleanroom serve` with in-process adapter wiring.
3. Implement unary RPCs first (`Create/Get/List/Terminate`, `Create/Get/Cancel`).
4. Add `StreamExecution` and `StreamSandboxEvents`.
5. Add `AttachExecution` bidi stream for interactive sessions.
6. Implement `cleanroom exec` as an RPC wrapper (`CreateSandbox` -> `CreateExecution` -> stream -> optional `TerminateSandbox`).
7. Wire remaining CLI subcommands to RPC client.
8. Add conformance tests for status transitions, error codes, signal handling, and stream termination behavior.

## 11) Out of Scope for Minimal v1

- Checkpoint/restore API
- Artifact upload API
- Multi-tenant org/authz policy model
- Cross-sandbox workflow orchestration

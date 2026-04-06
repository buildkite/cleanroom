# Cleanroom Minimal API (ConnectRPC)

## 1) Goal

Define a minimal, functional control API for creating and managing Cleanroom sandboxes with durable execution streaming and interactive execution bootstrap.

This document is intentionally small-scope:
- management plane for sandbox lifecycle
- execution plane for command runs and streaming output

## 2) Terminology

- `Sandbox`: A policy-bound execution environment created from an immutable `CompiledPolicy`.
- `Execution`: A single command invocation inside a sandbox.
- `Event`: Structured runtime and policy events emitted during sandbox/execution lifecycle.

`sandbox` is the top-level lifecycle noun. `execution_id` is the canonical identifier for a command invocation.

## 3) Transport Choice

Use ConnectRPC with HTTP/2 enabled in server deployments.

Why:
- unary RPCs for lifecycle operations
- server-streaming for durable batch execution output and event tails
- unary interactive bootstrap via `AttachExecution`, with QUIC carrying interactive PTY bytes and control frames after bootstrap

Non-goal for v1:
- REST endpoint support.
- ConnectRPC bidirectional streaming for interactive terminal traffic.

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

Default endpoint resolution:
- macOS: user socket `unix://$XDG_RUNTIME_DIR/cleanroom/cleanroom.sock`
- Linux: system socket when present `unix:///var/run/cleanroom/cleanroom.sock`, otherwise user socket

Fallbacks:
1. `--host` flag
2. `CLEANROOM_HOST`
3. runtime config `control_host`
4. default endpoint resolution

HTTP endpoints are also supported:
- `http://host:port` for direct HTTP
- `https://host:port` for TLS transport

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
2. `AttachExecution(AttachExecutionRequest) returns (AttachExecutionResponse)` (unary)
3. `GetExecution(GetExecutionRequest) returns (GetExecutionResponse)` (unary)
4. `InspectExecution(InspectExecutionRequest) returns (InspectExecutionResponse)` (unary)
5. `CancelExecution(CancelExecutionRequest) returns (CancelExecutionResponse)` (unary)
6. `WriteExecutionStdin(WriteExecutionStdinRequest) returns (WriteExecutionStdinResponse)` (unary)
7. `CloseExecutionStdin(CloseExecutionStdinRequest) returns (CloseExecutionStdinResponse)` (unary)
8. `StreamExecution(StreamExecutionRequest) returns (stream ExecutionStreamEvent)` (server-streaming)

`AttachExecution` is a bootstrap RPC for interactive executions. It returns a session token plus QUIC connection parameters. Interactive PTY bytes and control frames do not flow through ConnectRPC after bootstrap.

`StreamExecution` is the durable batch-oriented output stream. `InspectExecution` is the richer diagnostics surface for retained stdout/stderr, message, image metadata, artifacts dir, and observability payload.

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

import "google/protobuf/struct.proto";
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
  rpc AttachExecution(AttachExecutionRequest) returns (AttachExecutionResponse);
  rpc GetExecution(GetExecutionRequest) returns (GetExecutionResponse);
  rpc InspectExecution(InspectExecutionRequest) returns (InspectExecutionResponse);
  rpc CancelExecution(CancelExecutionRequest) returns (CancelExecutionResponse);
  rpc WriteExecutionStdin(WriteExecutionStdinRequest) returns (WriteExecutionStdinResponse);
  rpc CloseExecutionStdin(CloseExecutionStdinRequest) returns (CloseExecutionStdinResponse);
  rpc StreamExecution(StreamExecutionRequest) returns (stream ExecutionStreamEvent);
}

message Sandbox {
  string sandbox_id = 1;
  SandboxStatus status = 2;
  string backend = 3;
  string policy_hash = 4;
  google.protobuf.Timestamp created_at = 5;
  google.protobuf.Timestamp updated_at = 6;
  string last_execution_id = 7;
  string active_execution_id = 8;
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
  ExecutionKind kind = 10;
}

message AttachExecutionRequest {
  string sandbox_id = 1;
  string execution_id = 2;
  uint32 initial_cols = 3;
  uint32 initial_rows = 4;
}

message AttachExecutionResponse {
  string session_id = 1;
  string session_token = 2;
  google.protobuf.Timestamp expires_at = 3;
  string quic_endpoint = 4;
  string alpn = 5;
  string server_cert_pin_sha256 = 6;
}

message InspectExecutionRequest {
  string sandbox_id = 1;
  string execution_id = 2;
}

message InspectExecutionResponse {
  Execution execution = 1;
  string message = 2;
  string stdout = 3;
  string stderr = 4;
  string image_ref = 5;
  string image_digest = 6;
  string artifacts_dir = 7;
  string plan_path = 8;
  bool launched_vm = 9;
  google.protobuf.Struct observability = 10;
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

enum ExecutionKind {
  EXECUTION_KIND_UNSPECIFIED = 0;
  EXECUTION_KIND_BATCH = 1;
  EXECUTION_KIND_INTERACTIVE = 2;
}
```

## 9) CLI Subcommands

Expose a server mode and API-driven commands in the same binary.

### 9.1 Server

- `cleanroom serve --listen unix:///run/user/1000/cleanroom/cleanroom.sock`
- `cleanroom serve --listen http://127.0.0.1:7777`
- `cleanroom serve --listen https://127.0.0.1:7777 --tls-cert /path/to/server.pem --tls-key /path/to/server.key`

### 9.2 Sandbox commands

- `cleanroom sandbox create`
- `cleanroom sandbox create --from <snapshot-id>`
- `cleanroom sandbox inspect <sandbox-id>`
- `cleanroom sandbox ls`
- `cleanroom sandbox rm <sandbox-id>`

`sandbox inspect` is the primary way to discover a sandbox's `last_execution_id` and `active_execution_id`.

`cleanroom sandbox create` is repo-agnostic. Without `--from`, it does not
inspect the local git repository or read `cleanroom.yaml`; it synthesizes a
built-in policy from the selected image ref, uses deny-by-default networking by
default, enables the guest Docker service with `--docker`, and disables egress
filtering only when `--dangerously-allow-all` is set. `--image`, `--docker`,
and `--dangerously-allow-all` cannot be combined with `--from`.

### 9.3 Execution commands

- `cleanroom inspect <typeid>`
- `cleanroom execution inspect <execution-id>`
- `cleanroom execution inspect --sandbox-id <sandbox-id> --last`
- `cleanroom status --execution-id <execution-id>`
- `cleanroom status --last`

Notes:
- `sandbox inspect` is the lookup surface when you only know a sandbox ID.
- `execution inspect` is the live control-plane inspection surface.
- `status` is the local retained-artifacts browser under `$XDG_STATE_HOME/cleanroom/executions`.
- There are no first-class low-level `execution create/get/cancel/stream` CLI verbs; those are API/SDK operations.

### 9.4 Local UX

`cleanroom exec` remains the primary developer UX, but it is implemented as client/server RPC.

Default command:
- `cleanroom exec -- npm test`

Additional command forms:
- `cleanroom create`
- `cleanroom console -- bash`
- `cleanroom exec --in <sandbox-id> -- npm test`
- `cleanroom exec --from <snapshot-id> -- npm test`

Behavior contract:
1. Resolve server endpoint (`--host`, `CLEANROOM_HOST`, context, default unix socket).
2. Select sandbox mode:
   - `--in <id>` reuses an existing sandbox
   - `--from <snapshot-id>` creates a sandbox from a snapshot
   - otherwise create a sandbox from policy
3. When creating a sandbox from policy, resolve and compile repository policy.
4. If repo-aware bootstrap is enabled for that policy-created sandbox, resolve
   the current git remote and committed `HEAD`.
5. For repo-aware top-level commands, materialize that checkout in the sandbox
   and default the guest working directory to `repository.path`.
   - if the checkout contains `.mise.toml`, `mise.toml`, `.tool-versions`, or
     `.mise/config.toml`, execute the workload through
     `mise exec -- ...`, unless `sandbox.mise.enabled: false` or
     `sandbox.mise.install: false`
6. Create execution with explicit kind, env, and TTY options.
7. Attach stdio according to command mode:
   - `cleanroom exec` defaults to attached stdin plus separate stdout/stderr
     using `StreamExecution`, `WriteExecutionStdin`, and `CloseExecutionStdin`
   - `cleanroom exec --env KEY` inherits `KEY` from the local client
   - `cleanroom exec --env KEY=VALUE` sends an explicit guest env assignment
   - `cleanroom exec -n` closes stdin immediately so the command observes EOF
   - `cleanroom console` uses `AttachExecution` for PTY-oriented
     interactive sessions
8. Return the command exit code.
9. Newly created `exec` and `console` sandboxes are terminated after the command
   unless `--keep` is set. Reused sandboxes (`--in`) remain `READY`.

Notes:
- `cleanroom create`, `cleanroom exec`, and `cleanroom console` are the
  repo-aware UX layer by default.
- `cleanroom create --from <snapshot-id>` follows the snapshot path and does
  not read repository policy.
- `cleanroom sandbox create` stays generic and does not inspect the local git
  repository or read `cleanroom.yaml`.
- Dirty working trees warn and use committed `HEAD`; uncommitted changes are
  not copied into the sandbox.

Signal behavior:
1. First `Ctrl-C`: `CancelExecution`.
2. Second `Ctrl-C`: force detach client stream and return immediately.

Failure UX:
- Always print `sandbox_id` and `execution_id` when available.
- On `exec` or `console` failure, also print a ready-to-run `cleanroom execution inspect ...` follow-up command and `artifacts_dir` when available.
- On policy/runtime deny, print stable reason code and a follow-up command to inspect sandbox or execution state.

Debugging flow:
1. Start with `cleanroom sandbox inspect <sandbox-id>` when you need the sandbox state or do not yet know the execution ID.
2. Use `cleanroom execution inspect <execution-id>` for the canonical execution view.
3. Use `cleanroom status --execution-id <execution-id>` for retained local artifacts and raw observability files.

## 10) Suggested Implementation Plan

1. Define protobufs and generate ConnectRPC stubs.
2. Implement `cleanroom serve` with in-process adapter wiring.
3. Implement unary RPCs first (`Create/Get/List/Terminate`, `Create/Get/Cancel`).
4. Add `StreamExecution` and `StreamSandboxEvents`.
5. Add `AttachExecution`, `InspectExecution`, and stdin attach/close RPCs.
6. Implement `cleanroom exec` as an RPC wrapper (`CreateSandbox` ->
   `CreateExecution` -> optional stdin attach/close -> stream -> optional
   `TerminateSandbox`).
7. Wire remaining CLI subcommands to RPC client.
8. Add conformance tests for status transitions, error codes, signal handling, and stream termination behavior.

## 11) Out of Scope for Minimal v1

- Artifact upload API
- Multi-tenant org/authz policy model
- Cross-sandbox workflow orchestration

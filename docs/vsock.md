# Vsock Exec Protocol

The guest agent (`cleanroom-guest-agent`) listens on a vsock port (default `10700`) inside the Firecracker VM. The host connects over the Firecracker vsock UDS proxy to execute commands in the guest.

The protocol uses **newline-delimited JSON** over a single full-duplex vsock connection. Each connection handles one command execution.

## Connection Lifecycle

```
Host                                    Guest
 │                                        │
 │──── ExecRequest ──────────────────────>│  (1 JSON line)
 │                                        │
 │──── ExecInputFrame (stdin) ──────────>│  (0..n JSON lines)
 │──── ExecInputFrame (resize) ─────────>│
 │──── ExecInputFrame (eof) ────────────>│
 │                                        │
 │<──── ExecStreamFrame (stdout) ────────│  (0..n JSON lines)
 │<──── ExecStreamFrame (stderr) ────────│
 │<──── ExecStreamFrame (exit) ──────────│  (terminates session)
 │                                        │
```

Host→guest and guest→host streams are independent (full-duplex). Input frames and output frames flow concurrently.

## Messages

### ExecRequest (host → guest)

Sent once at the start of the connection.

```json
{
  "command": ["sh", "-c", "echo hello"],
  "dir": "/workspace",
  "env": ["FOO=bar"],
  "entropy_seed": "<base64>",
  "tty": true
}
```

| Field          | Type       | Required | Description                                      |
|----------------|------------|----------|--------------------------------------------------|
| `command`      | `string[]` | yes      | Command and arguments                            |
| `dir`          | `string`   | no       | Working directory                                |
| `env`          | `string[]` | no       | Environment variables (`KEY=value`)              |
| `entropy_seed` | `bytes`    | no       | Entropy to inject into guest `/dev/random`       |
| `tty`          | `bool`     | no       | Allocate a PTY for the command (default `false`) |

When `tty` is `true`, the guest allocates a pseudo-terminal. stdout and stderr are merged into a single PTY output stream (sent as `stdout` frames). Resize input frames control the terminal window size.

### ExecInputFrame (host → guest)

Sent after the request, zero or more times. Only processed if the guest agent version supports input frames; older agents ignore the host→guest direction after the request.

**stdin** — forward data to the process's stdin (or PTY):
```json
{"type": "stdin", "data": "<base64>"}
```

**resize** — change PTY window size (TTY mode only):
```json
{"type": "resize", "cols": 120, "rows": 40}
```

**eof** — signal end of stdin without closing the connection:
```json
{"type": "eof"}
```

### ExecStreamFrame (guest → host)

Sent zero or more times during command execution, terminated by an `exit` frame.

**stdout/stderr** — output chunk:
```json
{"type": "stdout", "data": "<base64>"}
{"type": "stderr", "data": "<base64>"}
```

In TTY mode, all output arrives as `stdout` (PTY merges streams).

The `data` field is base64-encoded bytes. The decoder also tolerates plain string values for resilience.

**exit** — command finished (final frame):
```json
{"type": "exit", "exit_code": 0, "error": ""}
```

| Field       | Type     | Description                                  |
|-------------|----------|----------------------------------------------|
| `exit_code` | `int`    | Process exit code (0 = success)              |
| `error`     | `string` | Guest-side error message, if any             |

### ExecResponse (legacy fallback)

If the guest agent fails to send stream frames (protocol mismatch), it falls back to a single JSON response:

```json
{"exit_code": 0, "stdout": "hello\n", "stderr": "", "error": ""}
```

The host decoder detects this by checking for the absence of a `type` field.

## Transport

- **Port:** Configurable via `CLEANROOM_VSOCK_PORT` env var in the guest, default `10700`
- **Encoding:** JSON with newline delimiter (`json.Encoder` / `json.Decoder`)
- **Binary data:** `[]byte` fields are base64-encoded by Go's `encoding/json`
- **Concurrency:** One command per connection. The guest agent accepts multiple sequential connections.

## Implementation

- Protocol types: `internal/vsockexec/protocol.go`
- Guest agent: `cmd/cleanroom-guest-agent/main.go`
- Host-side caller: `internal/backend/firecracker/backend.go` (`runGuestCommand`)

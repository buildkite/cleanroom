package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// ErrorCode is a stable classifier for cleanroom API errors.
type ErrorCode string

const (
	ErrorCodeUnknown                   ErrorCode = "unknown"
	ErrorCodeCanceled                  ErrorCode = "canceled"
	ErrorCodeDeadlineExceeded          ErrorCode = "deadline_exceeded"
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeNotFound                  ErrorCode = "not_found"
	ErrorCodeUnavailable               ErrorCode = "unavailable"
	ErrorCodeInternal                  ErrorCode = "internal"
	ErrorCodePolicyInvalid             ErrorCode = "policy_invalid"
	ErrorCodePolicyConflict            ErrorCode = "policy_conflict"
	ErrorCodeBackendUnavailable        ErrorCode = "backend_unavailable"
	ErrorCodeBackendCapabilityMismatch ErrorCode = "backend_capability_mismatch"
	ErrorCodeHostNotAllowed            ErrorCode = "host_not_allowed"
	ErrorCodeRegistryNotAllowed        ErrorCode = "registry_not_allowed"
	ErrorCodeLockfileViolation         ErrorCode = "lockfile_violation"
	ErrorCodeSecretScopeViolation      ErrorCode = "secret_scope_violation"
	ErrorCodeRuntimeLaunchFailed       ErrorCode = "runtime_launch_failed"
)

// ErrCode classifies API errors into a stable code.
//
// When the server returns a semantic app-level code in the error message,
// that code is preferred. Otherwise this falls back to transport-level
// Connect codes.
func ErrCode(err error) ErrorCode {
	if err == nil {
		return ErrorCodeUnknown
	}

	message := strings.ToLower(err.Error())
	if appCode := classifyAppErrorCode(message); appCode != ErrorCodeUnknown {
		return appCode
	}

	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		switch connectErr.Code() {
		case connect.CodeCanceled:
			return ErrorCodeCanceled
		case connect.CodeDeadlineExceeded:
			return ErrorCodeDeadlineExceeded
		case connect.CodeInvalidArgument:
			return ErrorCodeInvalidArgument
		case connect.CodeNotFound:
			return ErrorCodeNotFound
		case connect.CodeUnavailable:
			return ErrorCodeUnavailable
		default:
			return ErrorCodeInternal
		}
	}
	if errors.Is(err, context.Canceled) {
		return ErrorCodeCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorCodeDeadlineExceeded
	}
	return ErrorCodeUnknown
}

func classifyAppErrorCode(message string) ErrorCode {
	switch {
	case strings.Contains(message, string(ErrorCodePolicyInvalid)):
		return ErrorCodePolicyInvalid
	case strings.Contains(message, string(ErrorCodePolicyConflict)):
		return ErrorCodePolicyConflict
	case strings.Contains(message, string(ErrorCodeBackendUnavailable)):
		return ErrorCodeBackendUnavailable
	case strings.Contains(message, string(ErrorCodeBackendCapabilityMismatch)):
		return ErrorCodeBackendCapabilityMismatch
	case strings.Contains(message, string(ErrorCodeHostNotAllowed)):
		return ErrorCodeHostNotAllowed
	case strings.Contains(message, string(ErrorCodeRegistryNotAllowed)):
		return ErrorCodeRegistryNotAllowed
	case strings.Contains(message, string(ErrorCodeLockfileViolation)):
		return ErrorCodeLockfileViolation
	case strings.Contains(message, string(ErrorCodeSecretScopeViolation)):
		return ErrorCodeSecretScopeViolation
	case strings.Contains(message, string(ErrorCodeRuntimeLaunchFailed)):
		return ErrorCodeRuntimeLaunchFailed
	default:
		return ErrorCodeUnknown
	}
}

// Must returns the client if err is nil; otherwise it panics.
func Must(c *Client, err error) *Client {
	if err != nil {
		panic(err)
	}
	return c
}

// NewFromEnv builds a client from CLEANROOM_HOST (or default endpoint when unset).
func NewFromEnv(opts ...Option) (*Client, error) {
	return New("", opts...)
}

// HostPort models a policy allowlist destination.
type HostPort struct {
	Host  string
	Ports []int32
}

// Allow builds an allowlist destination entry for PolicyFromAllowlist.
func Allow(host string, ports ...int32) HostPort {
	trimmedHost := strings.TrimSpace(host)
	copiedPorts := make([]int32, 0, len(ports))
	for _, port := range ports {
		copiedPorts = append(copiedPorts, port)
	}
	return HostPort{Host: trimmedHost, Ports: copiedPorts}
}

// PolicyFromAllowlist creates a minimal deny-by-default policy with explicit allows.
func PolicyFromAllowlist(imageRef, imageDigest string, entries ...HostPort) *Policy {
	policy := &Policy{
		Version:        1,
		ImageRef:       strings.TrimSpace(imageRef),
		ImageDigest:    strings.TrimSpace(imageDigest),
		NetworkDefault: "deny",
		Allow:          make([]*PolicyAllowRule, 0, len(entries)),
	}
	for _, entry := range entries {
		host := strings.TrimSpace(entry.Host)
		if host == "" {
			continue
		}
		ports := make([]int32, len(entry.Ports))
		copy(ports, entry.Ports)
		if len(ports) == 0 {
			continue
		}
		policy.Allow = append(policy.Allow, &PolicyAllowRule{Host: host, Ports: ports})
	}
	return policy
}

// EnsureSandboxOptions controls EnsureSandbox behavior.
type EnsureSandboxOptions struct {
	Backend   string
	Policy    *Policy
	Options   *SandboxOptions
	SandboxID string
}

// SandboxHandle is a concise reusable sandbox descriptor.
type SandboxHandle struct {
	ID      string
	Backend string
	Status  SandboxStatus
	Created bool
}

// EnsureSandbox returns a reusable sandbox for a key.
//
// It reuses a previously tracked sandbox when present and still available.
// If opts.SandboxID is set, that sandbox is used directly and associated to key.
func (c *Client) EnsureSandbox(ctx context.Context, key string, opts EnsureSandboxOptions) (*SandboxHandle, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}

	trimmedKey := strings.TrimSpace(key)
	if trimmedKey == "" {
		return nil, errors.New("missing key")
	}
	unlockKey := c.lockEnsureKey(trimmedKey)
	defer unlockKey()

	if explicitID := strings.TrimSpace(opts.SandboxID); explicitID != "" {
		handle, err := c.fetchSandboxHandle(ctx, explicitID, false)
		if err != nil {
			return nil, err
		}
		c.recordSandboxKey(trimmedKey, explicitID)
		return handle, nil
	}

	if cachedID, ok := c.lookupSandboxKey(trimmedKey); ok {
		handle, err := c.fetchSandboxHandle(ctx, cachedID, false)
		if err == nil {
			if isReusableSandboxStatus(handle.Status) {
				return handle, nil
			}
			c.clearSandboxKey(trimmedKey)
		} else if ErrCode(err) != ErrorCodeNotFound {
			return nil, err
		} else {
			c.clearSandboxKey(trimmedKey)
		}
	}

	createReq := &CreateSandboxRequest{
		Backend: strings.TrimSpace(opts.Backend),
		Options: opts.Options,
		Policy:  opts.Policy,
	}
	if createReq.Policy == nil {
		return nil, errors.New("missing policy")
	}

	resp, err := c.CreateSandbox(ctx, createReq)
	if err != nil {
		return nil, err
	}
	sandbox := resp.GetSandbox()
	if sandbox == nil || strings.TrimSpace(sandbox.GetSandboxId()) == "" {
		return nil, errors.New("create sandbox returned empty sandbox_id")
	}
	c.recordSandboxKey(trimmedKey, sandbox.GetSandboxId())
	return &SandboxHandle{
		ID:      sandbox.GetSandboxId(),
		Backend: sandbox.GetBackend(),
		Status:  sandbox.GetStatus(),
		Created: true,
	}, nil
}

func isReusableSandboxStatus(status SandboxStatus) bool {
	return status == SandboxStatus_SANDBOX_STATUS_READY
}

func (c *Client) fetchSandboxHandle(ctx context.Context, sandboxID string, created bool) (*SandboxHandle, error) {
	resp, err := c.GetSandbox(ctx, &GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		return nil, err
	}
	sandbox := resp.GetSandbox()
	if sandbox == nil {
		return nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	return &SandboxHandle{
		ID:      sandbox.GetSandboxId(),
		Backend: sandbox.GetBackend(),
		Status:  sandbox.GetStatus(),
		Created: created,
	}, nil
}

func (c *Client) lookupSandboxKey(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.sandboxByKey[key]
	return id, ok
}

func (c *Client) recordSandboxKey(key, sandboxID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sandboxByKey[key] = sandboxID
}

func (c *Client) clearSandboxKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sandboxByKey, key)
}

func (c *Client) lockEnsureKey(key string) func() {
	c.mu.Lock()
	lock, ok := c.ensureLocks[key]
	if !ok {
		lock = &ensureKeyLock{}
		c.ensureLocks[key] = lock
	}
	lock.refs++
	c.mu.Unlock()

	lock.mu.Lock()

	return func() {
		lock.mu.Unlock()

		c.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(c.ensureLocks, key)
		}
		c.mu.Unlock()
	}
}

// ExecOptions controls how ExecAndWait streams command output.
type ExecOptions struct {
	Stdout io.Writer
	Stderr io.Writer
	TTY    bool
	// Timeout bounds the stream/wait phase after execution creation.
	Timeout time.Duration
}

// ExecResult is the final execution outcome from ExecAndWait.
type ExecResult struct {
	SandboxID   string
	ExecutionID string
	Status      ExecutionStatus
	ExitCode    int32
	Message     string
	Stdout      string
	Stderr      string
}

// ExecAndWait creates an execution, streams output, and waits for completion.
func (c *Client) ExecAndWait(ctx context.Context, sandboxID string, command []string, opts ExecOptions) (*ExecResult, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	if len(command) == 0 {
		return nil, errors.New("missing command")
	}

	createReq := &CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   append([]string(nil), command...),
	}
	if opts.TTY {
		createReq.Options = &ExecutionOptions{Tty: true}
	}

	created, err := c.CreateExecution(ctx, createReq)
	if err != nil {
		return nil, err
	}
	executionID := strings.TrimSpace(created.GetExecution().GetExecutionId())
	if executionID == "" {
		return nil, errors.New("create execution returned empty execution_id")
	}

	waitCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()

	stream, err := c.StreamExecution(waitCtx, &StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		c.cancelExecutionBestEffort(sandboxID, executionID)
		return nil, err
	}

	var (
		stdoutBuf bytes.Buffer
		stderrBuf bytes.Buffer
		status    = ExecutionStatus_EXECUTION_STATUS_UNSPECIFIED
		exitCode  int32
		message   string
	)

	for stream.Receive() {
		event := stream.Msg()
		if event == nil {
			continue
		}
		status = event.GetStatus()
		if chunk := event.GetStdout(); len(chunk) > 0 {
			_, _ = stdoutBuf.Write(chunk)
			if opts.Stdout != nil {
				_, _ = opts.Stdout.Write(chunk)
			}
		}
		if chunk := event.GetStderr(); len(chunk) > 0 {
			_, _ = stderrBuf.Write(chunk)
			if opts.Stderr != nil {
				_, _ = opts.Stderr.Write(chunk)
			}
		}
		if exit := event.GetExit(); exit != nil {
			exitCode = exit.GetExitCode()
			if exit.GetStatus() != ExecutionStatus_EXECUTION_STATUS_UNSPECIFIED {
				status = exit.GetStatus()
			}
			message = exit.GetMessage()
		}
		if msg := strings.TrimSpace(event.GetMessage()); msg != "" {
			message = msg
		}
	}
	if streamErr := stream.Err(); streamErr != nil {
		c.cancelExecutionBestEffort(sandboxID, executionID)
		return nil, streamErr
	}

	if status == ExecutionStatus_EXECUTION_STATUS_UNSPECIFIED {
		getResp, err := c.GetExecution(waitCtx, &GetExecutionRequest{SandboxId: sandboxID, ExecutionId: executionID})
		if err != nil {
			return nil, err
		}
		execution := getResp.GetExecution()
		if execution != nil {
			status = execution.GetStatus()
			exitCode = execution.GetExitCode()
		}
	}

	return &ExecResult{
		SandboxID:   sandboxID,
		ExecutionID: executionID,
		Status:      status,
		ExitCode:    exitCode,
		Message:     message,
		Stdout:      stdoutBuf.String(),
		Stderr:      stderrBuf.String(),
	}, nil
}

func (c *Client) cancelExecutionBestEffort(sandboxID, executionID string) {
	cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = c.CancelExecution(cancelCtx, &CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
}

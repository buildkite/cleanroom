package controlservice

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func executionRunErrorStatus(ex *executionState, runCtx context.Context) (cleanroomv1.ExecutionStatus, int32) {
	if ex == nil {
		return cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED, 1
	}
	if ex.CancelRequested || errors.Is(runCtx.Err(), context.Canceled) {
		return cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED, cancelExitCode(ex.CancelSignal)
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT, 124
	}
	return cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED, 1
}

func closeSandboxDoneLocked(sb *sandboxState) {
	if sb.DoneClosed {
		return
	}
	close(sb.Done)
	sb.DoneClosed = true
}

func closeExecutionDoneLocked(ex *executionState) {
	if ex.DoneClosed {
		return
	}
	close(ex.Done)
	ex.DoneClosed = true
}

func clearExecutionAttachIOLocked(ex *executionState) {
	if ex == nil {
		return
	}
	ex.AttachStdin = nil
	ex.AttachCloseStdin = nil
	ex.AttachResize = nil
}

func clearExecutionRuntimeHandlesLocked(ex *executionState) {
	if ex == nil {
		return
	}
	ex.Cancel = nil
	clearExecutionAttachIOLocked(ex)
}

func closeSandboxSubscribersLocked(sb *sandboxState) {
	sb.events.closeSubscribers()
}

func closeExecutionSubscribersLocked(ex *executionState) {
	ex.events.closeSubscribers()
}

func executionTerminalTime(ex *executionState) time.Time {
	if ex == nil {
		return time.Time{}
	}
	if ex.FinishedAt != nil {
		return *ex.FinishedAt
	}
	if ex.StartedAt != nil {
		return *ex.StartedAt
	}
	return time.Time{}
}

func executionSortTime(ex *executionState, sb *sandboxState) time.Time {
	if when := executionTerminalTime(ex); !when.IsZero() {
		return when
	}
	return sandboxTerminalTime(sb)
}

func sandboxTerminalTime(sb *sandboxState) time.Time {
	if sb == nil {
		return time.Time{}
	}
	if !sb.UpdatedAt.IsZero() {
		return sb.UpdatedAt
	}
	return sb.CreatedAt
}

func isFinalExecutionStatus(status cleanroomv1.ExecutionStatus) bool {
	switch status {
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT:
		return true
	default:
		return false
	}
}

func cancelExitCode(signal int32) int32 {
	if signal <= 0 || signal > 127 {
		return 130
	}
	return 128 + signal
}

func executionKey(sandboxID, executionID string) string {
	return sandboxID + "/" + executionID
}

func cloneSandboxLocked(state *sandboxState) *cleanroomv1.Sandbox {
	if state == nil {
		return nil
	}
	policyHash := ""
	if state.Policy != nil {
		policyHash = state.Policy.Hash
	}
	return &cleanroomv1.Sandbox{
		SandboxId:           state.ID,
		Status:              state.Status,
		Backend:             state.Backend,
		PolicyHash:          policyHash,
		CreatedAt:           timestamppb.New(state.CreatedAt),
		UpdatedAt:           timestamppb.New(state.UpdatedAt),
		LastExecutionId:     state.LastExecutionID,
		ActiveExecutionId:   state.ActiveExecutionID,
		SourceKind:          state.SourceKind,
		SourceId:            state.SourceID,
		BackingSnapshotId:   state.BackingSnapshotID,
		RepositoryCheckout:  cloneRepositoryCheckout(state.Repository).ToProto(),
		BackendCapabilities: backend.CloneCapabilities(state.Capabilities),
		EffectiveResources:  effectiveSandboxResources(state.Firecracker),
	}
}

func effectiveSandboxResources(cfg backend.FirecrackerConfig) *cleanroomv1.SandboxResources {
	resources := &cleanroomv1.SandboxResources{}
	if cfg.VCPUs > 0 {
		resources.Vcpus = cfg.VCPUs
	}
	if cfg.MemoryMiB > 0 {
		resources.MemoryBytes = cfg.MemoryMiB * mibBytes
	}
	if cfg.MinimumRootFSBytes > 0 {
		resources.DiskBytes = cfg.MinimumRootFSBytes
	}
	if resources.GetVcpus() == 0 && resources.GetMemoryBytes() == 0 && resources.GetDiskBytes() == 0 {
		return nil
	}
	return resources
}

func cloneExecutionLocked(state *executionState) *cleanroomv1.Execution {
	if state == nil {
		return nil
	}
	out := &cleanroomv1.Execution{
		ExecutionId: state.ID,
		SandboxId:   state.SandboxID,
		Status:      state.Status,
		Command:     append([]string(nil), state.Command...),
		ExitCode:    state.ExitCode,
		Tty:         state.TTY,
		Kind:        state.Kind,
	}
	if state.StartedAt != nil {
		out.StartedAt = timestamppb.New(*state.StartedAt)
	}
	if state.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*state.FinishedAt)
	}
	return out
}

func resolveExecutionKind(kind cleanroomv1.ExecutionKind, tty bool) (cleanroomv1.ExecutionKind, error) {
	if kind == cleanroomv1.ExecutionKind_EXECUTION_KIND_UNSPECIFIED {
		if tty {
			return cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE, nil
		}
		return cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH, nil
	}
	switch kind {
	case cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH:
		return kind, nil
	case cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE:
		return kind, nil
	default:
		return cleanroomv1.ExecutionKind_EXECUTION_KIND_UNSPECIFIED, fmt.Errorf("unsupported execution kind %q", kind.String())
	}
}

func newSessionToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func normalizeCommand(command []string) []string {
	if len(command) > 0 && command[0] == "--" {
		return command[1:]
	}
	return command
}

func normalizeExecutionEnv(env []string) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}

	out := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.Contains(entry, "\x00") {
			return nil, fmt.Errorf("invalid env entry %q: contains NUL", entry)
		}
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("invalid env entry %q: expected KEY=VALUE", entry)
		}
		if key == "" {
			return nil, fmt.Errorf("invalid env entry %q: missing variable name", entry)
		}
		out = append(out, entry)
	}
	return out, nil
}

func appendRetainedOutput(existing, chunk string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if chunk == "" {
		return retainedUTF8Tail(existing, limit)
	}
	chunk = strings.ToValidUTF8(chunk, "\uFFFD")
	if len(chunk) >= limit {
		return retainedUTF8Tail(chunk, limit)
	}
	keepExisting := limit - len(chunk)
	existing = retainedUTF8Tail(existing, keepExisting)
	return retainedUTF8Tail(existing+chunk, limit)
}

func retainedUTF8Tail(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return strings.Clone(value)
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return strings.Clone(value[start:])
}

func appendBounded[T any](history []T, item T, limit int) []T {
	if limit <= 0 {
		return nil
	}
	history = append(history, item)
	if len(history) <= limit {
		return history
	}
	trimmed := make([]T, limit)
	copy(trimmed, history[len(history)-limit:])
	return trimmed
}

package controlservice

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/authz"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type interactiveSessionGrant struct {
	SessionID           string
	SessionToken        string
	ExpiresAt           time.Time
	QuicEndpoint        string
	Alpn                string
	ServerCertPinSHA256 string
}

type interactiveSessionBroker struct {
	sessions map[string]*interactiveSessionState
	attached map[string]struct{}
	endpoint string
	alpn     string
	certPin  string
}

func (b *interactiveSessionBroker) ensureMaps() {
	if b.sessions == nil {
		b.sessions = map[string]*interactiveSessionState{}
	}
	if b.attached == nil {
		b.attached = map[string]struct{}{}
	}
}

func (b *interactiveSessionBroker) configureTransport(endpoint, alpn, certPinSHA256 string) {
	b.endpoint = strings.TrimSpace(endpoint)
	b.alpn = strings.TrimSpace(alpn)
	b.certPin = strings.TrimSpace(certPinSHA256)
}

func (b *interactiveSessionBroker) open(
	executions map[string]*executionState,
	now time.Time,
	ttl time.Duration,
	sessionID, token, sandboxID, executionID string,
	owner authz.ResourceOwner,
	initialCols, initialRows uint32,
) (*interactiveSessionGrant, error) {
	b.ensureMaps()
	b.pruneExpired(now)

	if b.endpoint == "" || b.alpn == "" {
		return nil, errors.New("interactive transport is not configured")
	}

	ex, ok := executions[executionKey(sandboxID, executionID)]
	if !ok {
		return nil, fmt.Errorf("unknown execution %q in sandbox %q", executionID, sandboxID)
	}
	if ex.Kind != cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE {
		return nil, fmt.Errorf("execution %q is not interactive", executionID)
	}
	if isFinalExecutionStatus(ex.Status) {
		return nil, fmt.Errorf("execution %q is no longer active", executionID)
	}

	execKey := executionKey(sandboxID, executionID)
	if _, attached := b.attached[execKey]; attached {
		return nil, fmt.Errorf("execution %q already has an active interactive session", executionID)
	}
	if b.hasPending(execKey) {
		return nil, fmt.Errorf("execution %q already has a pending interactive session", executionID)
	}

	expiresAt := now.Add(ttl)
	b.sessions[sessionID] = &interactiveSessionState{
		SessionID:   sessionID,
		SandboxID:   sandboxID,
		ExecutionID: executionID,
		Token:       token,
		Owner:       owner,
		ExpiresAt:   expiresAt,
		InitialCols: initialCols,
		InitialRows: initialRows,
	}

	return &interactiveSessionGrant{
		SessionID:           sessionID,
		SessionToken:        token,
		ExpiresAt:           expiresAt,
		QuicEndpoint:        b.endpoint,
		Alpn:                b.alpn,
		ServerCertPinSHA256: b.certPin,
	}, nil
}

func (b *interactiveSessionBroker) consume(
	executions map[string]*executionState,
	now time.Time,
	sessionID, token string,
) (*InteractiveSession, error) {
	b.ensureMaps()
	b.pruneExpired(now)

	session, ok := b.sessions[sessionID]
	if !ok || session == nil {
		return nil, fmt.Errorf("unknown interactive session %q", sessionID)
	}
	if session.Token != token {
		return nil, errors.New("invalid session token")
	}

	execKey := executionKey(session.SandboxID, session.ExecutionID)
	ex, ok := executions[execKey]
	if !ok {
		delete(b.sessions, sessionID)
		return nil, fmt.Errorf("unknown execution %q in sandbox %q", session.ExecutionID, session.SandboxID)
	}
	if ex.Kind != cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE {
		delete(b.sessions, sessionID)
		return nil, fmt.Errorf("execution %q is not interactive", session.ExecutionID)
	}
	if isFinalExecutionStatus(ex.Status) {
		delete(b.sessions, sessionID)
		return nil, fmt.Errorf("execution %q is no longer active", session.ExecutionID)
	}
	if _, attached := b.attached[execKey]; attached {
		delete(b.sessions, sessionID)
		return nil, fmt.Errorf("execution %q already has an active interactive session", session.ExecutionID)
	}

	delete(b.sessions, sessionID)
	b.attached[execKey] = struct{}{}
	return &InteractiveSession{
		SessionID:   session.SessionID,
		SandboxID:   session.SandboxID,
		ExecutionID: session.ExecutionID,
		InitialCols: session.InitialCols,
		InitialRows: session.InitialRows,
	}, nil
}

func (b *interactiveSessionBroker) release(sandboxID, executionID string) {
	b.ensureMaps()
	delete(b.attached, executionKey(sandboxID, executionID))
}

func (b *interactiveSessionBroker) clearExecution(execKey string) {
	b.ensureMaps()
	delete(b.attached, execKey)
	for id, session := range b.sessions {
		if session == nil {
			delete(b.sessions, id)
			continue
		}
		if executionKey(session.SandboxID, session.ExecutionID) == execKey {
			delete(b.sessions, id)
		}
	}
}

func (b *interactiveSessionBroker) pruneExpired(now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for id, session := range b.sessions {
		if session == nil || now.After(session.ExpiresAt) {
			delete(b.sessions, id)
		}
	}
}

func (b *interactiveSessionBroker) hasPending(execKey string) bool {
	for _, session := range b.sessions {
		if session == nil {
			continue
		}
		if executionKey(session.SandboxID, session.ExecutionID) == execKey {
			return true
		}
	}
	return false
}

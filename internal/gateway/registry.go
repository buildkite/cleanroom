package gateway

import (
	"fmt"
	"strings"
	"sync"

	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/policy"
	"go.opentelemetry.io/otel/trace"
)

// SandboxScope holds the identity and policy for a registered sandbox.
type SandboxScope struct {
	SandboxID    string
	GuestIP      string
	Policy       *policy.CompiledPolicy
	GatewayScope gatewayauth.ScopeMetadata
	ExecutionID  string
	TraceContext trace.SpanContext
}

// Registry is a thread-safe mapping of guest IPs to sandbox scopes.
type Registry struct {
	mu           sync.RWMutex
	byGuestIP    map[string]*SandboxScope
	byScopeToken map[string]*SandboxScope
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byGuestIP:    make(map[string]*SandboxScope),
		byScopeToken: make(map[string]*SandboxScope),
	}
}

// Register adds a sandbox scope keyed by guest IP. Returns an error if the IP
// is already registered (possible hash collision).
func (r *Registry) Register(guestIP, sandboxID string, p *policy.CompiledPolicy, metadata ...gatewayauth.ScopeMetadata) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byGuestIP[guestIP]; exists {
		return fmt.Errorf("guest IP %s already registered (possible IP collision)", guestIP)
	}
	r.byGuestIP[guestIP] = &SandboxScope{
		SandboxID:    sandboxID,
		GuestIP:      guestIP,
		Policy:       p,
		GatewayScope: firstScopeMetadata(metadata).Clone(),
	}
	return nil
}

// Release removes a sandbox scope by guest IP.
func (r *Registry) Release(guestIP string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byGuestIP, guestIP)
}

// Lookup retrieves a sandbox scope by guest IP.
func (r *Registry) Lookup(guestIP string) (*SandboxScope, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	scope, ok := r.byGuestIP[guestIP]
	return cloneSandboxScope(scope), ok
}

// RegisterScopeToken adds a sandbox scope keyed by a capability token.
// Returns an error if the token is already registered.
func (r *Registry) RegisterScopeToken(scopeToken, sandboxID string, p *policy.CompiledPolicy, metadata ...gatewayauth.ScopeMetadata) error {
	scopeToken = strings.TrimSpace(scopeToken)
	if scopeToken == "" {
		return fmt.Errorf("scope token must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byScopeToken[scopeToken]; exists {
		return fmt.Errorf("scope token already registered")
	}
	r.byScopeToken[scopeToken] = &SandboxScope{
		SandboxID:    sandboxID,
		Policy:       p,
		GatewayScope: firstScopeMetadata(metadata).Clone(),
	}
	return nil
}

func firstScopeMetadata(values []gatewayauth.ScopeMetadata) gatewayauth.ScopeMetadata {
	if len(values) == 0 {
		return gatewayauth.ScopeMetadata{}
	}
	return values[0]
}

// ReleaseScopeToken removes a sandbox scope by token.
func (r *Registry) ReleaseScopeToken(scopeToken string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byScopeToken, scopeToken)
}

// LookupScopeToken retrieves a sandbox scope by token.
func (r *Registry) LookupScopeToken(scopeToken string) (*SandboxScope, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	scope, ok := r.byScopeToken[scopeToken]
	return cloneSandboxScope(scope), ok
}

func (r *Registry) SetActiveExecutionTrace(sandboxID, executionID string, spanContext trace.SpanContext) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return
	}
	executionID = strings.TrimSpace(executionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, scope := range r.byGuestIP {
		setActiveExecutionTraceForScope(scope, sandboxID, executionID, spanContext)
	}
	for _, scope := range r.byScopeToken {
		setActiveExecutionTraceForScope(scope, sandboxID, executionID, spanContext)
	}
}

func (r *Registry) ClearActiveExecutionTrace(sandboxID, executionID string) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return
	}
	executionID = strings.TrimSpace(executionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, scope := range r.byGuestIP {
		clearActiveExecutionTraceForScope(scope, sandboxID, executionID)
	}
	for _, scope := range r.byScopeToken {
		clearActiveExecutionTraceForScope(scope, sandboxID, executionID)
	}
}

func cloneSandboxScope(scope *SandboxScope) *SandboxScope {
	if scope == nil {
		return nil
	}
	clone := *scope
	clone.GatewayScope = scope.GatewayScope.Clone()
	return &clone
}

func setActiveExecutionTraceForScope(scope *SandboxScope, sandboxID, executionID string, spanContext trace.SpanContext) {
	if scope == nil || scope.SandboxID != sandboxID {
		return
	}
	scope.ExecutionID = executionID
	scope.TraceContext = spanContext
}

func clearActiveExecutionTraceForScope(scope *SandboxScope, sandboxID, executionID string) {
	if scope == nil || scope.SandboxID != sandboxID {
		return
	}
	if executionID != "" && strings.TrimSpace(scope.ExecutionID) != executionID {
		return
	}
	scope.ExecutionID = ""
	scope.TraceContext = trace.SpanContext{}
}

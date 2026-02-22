package gateway

import (
	"fmt"
	"sync"

	"github.com/buildkite/cleanroom/internal/policy"
)

// SandboxScope holds the identity and policy for a registered sandbox.
type SandboxScope struct {
	SandboxID string
	GuestIP   string
	Policy    *policy.CompiledPolicy
}

// Registry is a thread-safe mapping of guest IPs to sandbox scopes.
type Registry struct {
	mu        sync.RWMutex
	byGuestIP map[string]*SandboxScope
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{byGuestIP: make(map[string]*SandboxScope)}
}

// Register adds a sandbox scope keyed by guest IP. Returns an error if the IP
// is already registered (possible hash collision).
func (r *Registry) Register(guestIP, sandboxID string, p *policy.CompiledPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byGuestIP[guestIP]; exists {
		return fmt.Errorf("guest IP %s already registered (possible IP collision)", guestIP)
	}
	r.byGuestIP[guestIP] = &SandboxScope{
		SandboxID: sandboxID,
		GuestIP:   guestIP,
		Policy:    p,
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
	return scope, ok
}

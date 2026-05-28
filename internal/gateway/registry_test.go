package gateway

import (
	"sync"
	"testing"

	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/policy"
)

func testPolicy() *policy.CompiledPolicy {
	return &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          []policy.AllowRule{{Host: "github.com", Ports: []int{443}}},
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	p := testPolicy()
	if err := r.Register("10.1.1.2", "sandbox-1", p); err != nil {
		t.Fatalf("register: %v", err)
	}

	scope, ok := r.Lookup("10.1.1.2")
	if !ok {
		t.Fatal("expected lookup to succeed")
	}
	if scope.SandboxID != "sandbox-1" {
		t.Fatalf("expected sandbox-1, got %s", scope.SandboxID)
	}
	if scope.GuestIP != "10.1.1.2" {
		t.Fatalf("expected 10.1.1.2, got %s", scope.GuestIP)
	}
	if scope.Policy != p {
		t.Fatal("policy mismatch")
	}
}

func TestRegistryRegisterCopiesGatewayScopeMetadata(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	metadata := gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{
			PrincipalID: "oidc:test:alice",
			Scope:       "repo:buildkite/cleanroom",
		},
		Authorization: gatewayauth.Authorization{
			GitRepoPrefixes: []string{"github.com/buildkite/cleanroom"},
			OCIRepoPrefixes: []string{"ghcr.io/buildkite/cleanroom-base/alpine"},
		},
	}
	if err := r.Register("10.1.1.2", "sandbox-1", testPolicy(), metadata); err != nil {
		t.Fatalf("register: %v", err)
	}
	metadata.Authorization.GitRepoPrefixes[0] = "github.com/other/repo"

	scope, ok := r.Lookup("10.1.1.2")
	if !ok {
		t.Fatal("expected lookup to succeed")
	}
	if got, want := scope.GatewayScope.Owner.PrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("unexpected owner: got %q want %q", got, want)
	}
	if got, want := scope.GatewayScope.Authorization.GitRepoPrefixes[0], "github.com/buildkite/cleanroom"; got != want {
		t.Fatalf("unexpected git authorization: got %q want %q", got, want)
	}
	scope.GatewayScope.Authorization.GitRepoPrefixes[0] = "github.com/mutated/repo"
	scope, _ = r.Lookup("10.1.1.2")
	if got, want := scope.GatewayScope.Authorization.GitRepoPrefixes[0], "github.com/buildkite/cleanroom"; got != want {
		t.Fatalf("lookup returned mutable metadata: got %q want %q", got, want)
	}
}

func TestRegistryDuplicateIPReturnsError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.Register("10.1.1.2", "sandbox-1", testPolicy()); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register("10.1.1.2", "sandbox-2", testPolicy()); err == nil {
		t.Fatal("expected error on duplicate IP registration")
	}
}

func TestRegistryRelease(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.Register("10.1.1.2", "sandbox-1", testPolicy()); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.Release("10.1.1.2")

	if _, ok := r.Lookup("10.1.1.2"); ok {
		t.Fatal("expected lookup to fail after release")
	}
}

func TestRegistryReleaseUnknownIsNoop(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Release("10.99.99.99") // should not panic
}

func TestRegistryLookupUnregisteredReturnsFalse(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if _, ok := r.Lookup("10.1.1.2"); ok {
		t.Fatal("expected lookup to return false for unregistered IP")
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := "10.0.0." + itoa(i%256)
			id := "sandbox-" + itoa(i)
			_ = r.Register(ip, id, testPolicy())
			r.Lookup(ip)
			r.Release(ip)
		}(i)
	}
	wg.Wait()
}

func TestRegistryReRegisterAfterRelease(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.Register("10.1.1.2", "sandbox-1", testPolicy()); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.Release("10.1.1.2")

	if err := r.Register("10.1.1.2", "sandbox-2", testPolicy()); err != nil {
		t.Fatalf("re-register after release: %v", err)
	}
	scope, ok := r.Lookup("10.1.1.2")
	if !ok || scope.SandboxID != "sandbox-2" {
		t.Fatal("re-registered scope mismatch")
	}
}

func TestRegistryRegisterScopeTokenAndLookup(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	p := testPolicy()
	if err := r.RegisterScopeToken("token-1", "sandbox-1", p); err != nil {
		t.Fatalf("register scope token: %v", err)
	}

	scope, ok := r.LookupScopeToken("token-1")
	if !ok {
		t.Fatal("expected token lookup to succeed")
	}
	if scope.SandboxID != "sandbox-1" {
		t.Fatalf("expected sandbox-1, got %s", scope.SandboxID)
	}
	if scope.Policy != p {
		t.Fatal("policy mismatch")
	}
}

func TestRegistryRegisterScopeTokenCopiesGatewayScopeMetadata(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	metadata := gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{PrincipalID: "oidc:test:alice"},
		Authorization: gatewayauth.Authorization{
			GitRepoPrefixes: []string{"github.com/buildkite/cleanroom"},
		},
	}
	if err := r.RegisterScopeToken("token-1", "sandbox-1", testPolicy(), metadata); err != nil {
		t.Fatalf("register scope token: %v", err)
	}

	scope, ok := r.LookupScopeToken("token-1")
	if !ok {
		t.Fatal("expected token lookup to succeed")
	}
	if got, want := scope.GatewayScope.Owner.PrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("unexpected owner: got %q want %q", got, want)
	}
}

func TestRegistryDuplicateScopeTokenReturnsError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.RegisterScopeToken("token-1", "sandbox-1", testPolicy()); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.RegisterScopeToken("token-1", "sandbox-2", testPolicy()); err == nil {
		t.Fatal("expected error on duplicate token registration")
	}
}

func TestRegistryEmptyScopeTokenReturnsError(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.RegisterScopeToken("   ", "sandbox-1", testPolicy()); err == nil {
		t.Fatal("expected error on empty token registration")
	}
}

func TestRegistryReleaseScopeToken(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.RegisterScopeToken("token-1", "sandbox-1", testPolicy()); err != nil {
		t.Fatalf("register scope token: %v", err)
	}
	r.ReleaseScopeToken("token-1")

	if _, ok := r.LookupScopeToken("token-1"); ok {
		t.Fatal("expected token lookup to fail after release")
	}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

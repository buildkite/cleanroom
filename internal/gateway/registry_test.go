package gateway

import (
	"sync"
	"testing"

	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/policy"
	"go.opentelemetry.io/otel/trace"
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

func TestRegistrySetActiveExecutionScopeUpdatesGatewayScope(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.Register("10.1.1.2", "sandbox-1", testPolicy(), gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{PrincipalID: "oidc:test:old"},
	}); err != nil {
		t.Fatalf("register scope: %v", err)
	}

	r.SetActiveExecutionScope("sandbox-1", "exec-1", trace.SpanContext{}, gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{PrincipalID: "oidc:test:alice"},
		Authorization: gatewayauth.Authorization{
			GitRepoPrefixes: []string{"github.com/buildkite/cleanroom"},
		},
	})

	scope, ok := r.Lookup("10.1.1.2")
	if !ok {
		t.Fatal("expected scope lookup to succeed")
	}
	if got, want := scope.ExecutionID, "exec-1"; got != want {
		t.Fatalf("execution id mismatch: got %q want %q", got, want)
	}
	if got, want := scope.GatewayScope.Owner.PrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("gateway owner mismatch: got %q want %q", got, want)
	}
	if got, want := scope.GatewayScope.Authorization.GitRepoPrefixes[0], "github.com/buildkite/cleanroom"; got != want {
		t.Fatalf("gateway git prefix mismatch: got %q want %q", got, want)
	}
}

func TestRegistryClearActiveExecutionScopeRestoresRegisteredGatewayScope(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	registered := gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{PrincipalID: "oidc:test:old"},
		Authorization: gatewayauth.Authorization{
			GitRepoPrefixes: []string{"github.com/buildkite/original"},
			OCIRepoPrefixes: []string{"ghcr.io/buildkite/original"},
		},
	}
	if err := r.Register("10.1.1.2", "sandbox-1", testPolicy(), registered); err != nil {
		t.Fatalf("register scope: %v", err)
	}
	if err := r.RegisterScopeToken("token-1", "sandbox-1", testPolicy(), registered); err != nil {
		t.Fatalf("register scope token: %v", err)
	}

	r.SetActiveExecutionScope("sandbox-1", "exec-1", testSpanContext(), gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{PrincipalID: "oidc:test:alice"},
		Authorization: gatewayauth.Authorization{
			GitRepoPrefixes: []string{"github.com/buildkite/cleanroom"},
		},
	})
	r.ClearActiveExecutionScope("sandbox-1", "exec-1")

	for name, scope := range map[string]*SandboxScope{
		"guest IP":    mustLookupScope(t, r, "10.1.1.2"),
		"scope token": mustLookupScopeToken(t, r, "token-1"),
	} {
		if got := scope.ExecutionID; got != "" {
			t.Fatalf("%s execution id mismatch: got %q want empty", name, got)
		}
		if scope.TraceContext.IsValid() {
			t.Fatalf("%s trace context should be cleared", name)
		}
		if got, want := scope.GatewayScope.Owner.PrincipalID, "oidc:test:old"; got != want {
			t.Fatalf("%s gateway owner mismatch: got %q want %q", name, got, want)
		}
		if got, want := scope.GatewayScope.Authorization.GitRepoPrefixes[0], "github.com/buildkite/original"; got != want {
			t.Fatalf("%s gateway git prefix mismatch: got %q want %q", name, got, want)
		}
		if got, want := scope.GatewayScope.Authorization.OCIRepoPrefixes[0], "ghcr.io/buildkite/original"; got != want {
			t.Fatalf("%s gateway oci prefix mismatch: got %q want %q", name, got, want)
		}
	}
}

func TestRegistryClearActiveExecutionScopeIgnoresMismatchedExecution(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	if err := r.Register("10.1.1.2", "sandbox-1", testPolicy(), gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{PrincipalID: "oidc:test:old"},
	}); err != nil {
		t.Fatalf("register scope: %v", err)
	}
	r.SetActiveExecutionScope("sandbox-1", "exec-1", testSpanContext(), gatewayauth.ScopeMetadata{
		Owner: gatewayauth.Owner{PrincipalID: "oidc:test:alice"},
	})
	r.ClearActiveExecutionScope("sandbox-1", "exec-other")

	scope := mustLookupScope(t, r, "10.1.1.2")
	if got, want := scope.ExecutionID, "exec-1"; got != want {
		t.Fatalf("execution id mismatch: got %q want %q", got, want)
	}
	if !scope.TraceContext.IsValid() {
		t.Fatal("trace context should still be active")
	}
	if got, want := scope.GatewayScope.Owner.PrincipalID, "oidc:test:alice"; got != want {
		t.Fatalf("gateway owner mismatch: got %q want %q", got, want)
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

func mustLookupScope(t *testing.T, r *Registry, guestIP string) *SandboxScope {
	t.Helper()
	scope, ok := r.Lookup(guestIP)
	if !ok {
		t.Fatalf("expected lookup for guest IP %q to succeed", guestIP)
	}
	return scope
}

func mustLookupScopeToken(t *testing.T, r *Registry, scopeToken string) *SandboxScope {
	t.Helper()
	scope, ok := r.LookupScopeToken(scopeToken)
	if !ok {
		t.Fatalf("expected lookup for scope token %q to succeed", scopeToken)
	}
	return scope
}

func testSpanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

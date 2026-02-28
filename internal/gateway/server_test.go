package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestExtractSourceIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"10.1.1.2:43210", "10.1.1.2"},
		{"[::ffff:10.1.1.2]:43210", "10.1.1.2"},
		{"192.168.1.1:8080", "192.168.1.1"},
		{"[::1]:9090", "::1"},
		{"10.1.1.2", "10.1.1.2"},
	}
	for _, tt := range tests {
		got := extractSourceIP(tt.input)
		if got != tt.want {
			t.Errorf("extractSourceIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIdentityMiddleware403ForUnregisteredIP(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	srv := &Server{registry: reg}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/git/github.com/org/repo", nil)
	req.RemoteAddr = "10.99.99.99:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestIdentityMiddlewareInjectsScope(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.Register("10.1.1.2", "sandbox-1", p); err != nil {
		t.Fatalf("register: %v", err)
	}

	srv := &Server{registry: reg}
	var gotScope *SandboxScope
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotScope, _ = ScopeFromContext(r.Context())
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.1.1.2:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotScope == nil {
		t.Fatal("expected scope in context")
	}
	if gotScope.SandboxID != "sandbox-1" {
		t.Fatalf("expected sandbox-1, got %s", gotScope.SandboxID)
	}
}

func TestIdentityMiddlewareFallsBackToScopeToken(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.RegisterScopeToken("token-1", "sandbox-token", p); err != nil {
		t.Fatalf("register scope token: %v", err)
	}

	srv := &Server{registry: reg}
	var gotScope *SandboxScope
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotScope, _ = ScopeFromContext(r.Context())
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "172.16.1.100:12345"
	req.Header.Set(ScopeTokenHeader, "token-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotScope == nil {
		t.Fatal("expected scope in context")
	}
	if gotScope.SandboxID != "sandbox-token" {
		t.Fatalf("expected sandbox-token, got %s", gotScope.SandboxID)
	}
}

func TestIdentityMiddlewareRejectsUnknownScopeToken(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	srv := &Server{registry: reg}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := srv.identityMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "172.16.1.100:12345"
	req.Header.Set(ScopeTokenHeader, "unknown-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestPathMiddlewareRejectsTraversal(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := srv.pathMiddleware(inner)

	req := httptest.NewRequest("GET", "/git/../secrets/key", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestStubHandlerReturns501(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	p := &policy.CompiledPolicy{Version: 1, NetworkDefault: "deny"}
	if err := reg.Register("10.1.1.2", "sandbox-1", p); err != nil {
		t.Fatalf("register: %v", err)
	}

	srv := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0",
		Registry:   reg,
	})

	req := httptest.NewRequest("GET", "/secrets/key", nil)
	req.RemoteAddr = "10.1.1.2:12345"
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if got := string(body); got != "secrets service not yet implemented" {
		t.Fatalf("unexpected body: %q", got)
	}
}

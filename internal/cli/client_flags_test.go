package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestClientFlagsResolvedHostPrefersExplicitHost(t *testing.T) {
	t.Parallel()

	flags := clientFlags{Host: "unix:///tmp/explicit.sock"}
	cfg := runtimeconfig.Config{ControlHost: "unix:///tmp/from-config.sock"}

	if got, want := flags.resolvedHost(cfg), "unix:///tmp/explicit.sock"; got != want {
		t.Fatalf("resolvedHost mismatch: got %q want %q", got, want)
	}
}

func TestClientFlagsResolvedHostUsesConfigFallback(t *testing.T) {
	t.Parallel()

	flags := clientFlags{}
	cfg := runtimeconfig.Config{ControlHost: "unix:///tmp/from-config.sock"}

	if got, want := flags.resolvedHost(cfg), "unix:///tmp/from-config.sock"; got != want {
		t.Fatalf("resolvedHost mismatch: got %q want %q", got, want)
	}
}

func TestClientFlagsResolvedHostReturnsEmptyWhenUnset(t *testing.T) {
	t.Parallel()

	flags := clientFlags{}
	cfg := runtimeconfig.Config{}

	if got := flags.resolvedHost(cfg); got != "" {
		t.Fatalf("resolvedHost mismatch: got %q want empty", got)
	}
}

func TestClientFlagsResolveAuthTokenFromDefaultEnv(t *testing.T) {
	t.Setenv("CLEANROOM_AUTH_TOKEN", " token-value ")

	token, err := (&clientFlags{}).resolveAuthToken(&runtimeContext{})
	if err != nil {
		t.Fatalf("resolveAuthToken returned error: %v", err)
	}
	if got, want := token, "token-value"; got != want {
		t.Fatalf("token mismatch: got %q want %q", got, want)
	}
}

func TestClientFlagsResolveAuthTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token.jwt"), []byte(" file-token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	token, err := (&clientFlags{AuthTokenFile: "token.jwt"}).resolveAuthToken(&runtimeContext{CWD: dir})
	if err != nil {
		t.Fatalf("resolveAuthToken returned error: %v", err)
	}
	if got, want := token, "file-token"; got != want {
		t.Fatalf("token mismatch: got %q want %q", got, want)
	}
}

func TestClientFlagsResolveAuthTokenErrorsForMissingExplicitEnv(t *testing.T) {
	_, err := (&clientFlags{AuthTokenEnv: "CLEANROOM_TEST_MISSING_TOKEN"}).resolveAuthToken(&runtimeContext{})
	if err == nil || !strings.Contains(err.Error(), "CLEANROOM_TEST_MISSING_TOKEN") {
		t.Fatalf("resolveAuthToken error = %v, want missing explicit env", err)
	}
}

func TestValidateBearerAuthListenEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ep      endpoint.Endpoint
		wantErr bool
	}{
		{
			name: "loopback http",
			ep:   endpoint.Endpoint{Scheme: "http", Address: "http://127.0.0.1:7777"},
		},
		{
			name: "localhost http",
			ep:   endpoint.Endpoint{Scheme: "http", Address: "http://localhost:7777"},
		},
		{
			name: "public https",
			ep:   endpoint.Endpoint{Scheme: "https", Address: "https://0.0.0.0:7777"},
		},
		{
			name:    "public http",
			ep:      endpoint.Endpoint{Scheme: "http", Address: "http://0.0.0.0:7777"},
			wantErr: true,
		},
		{
			name:    "wildcard ipv6 http",
			ep:      endpoint.Endpoint{Scheme: "http", Address: "http://[::]:7777"},
			wantErr: true,
		},
		{
			name:    "remote http",
			ep:      endpoint.Endpoint{Scheme: "http", Address: "http://cleanroom.example.com:7777"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateBearerAuthListenEndpoint(tt.ep)
			if tt.wantErr && err == nil {
				t.Fatal("expected endpoint validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateBearerAuthListenEndpoint returned error: %v", err)
			}
		})
	}
}

func TestValidateControlPlaneListenAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ep      endpoint.Endpoint
		cfg     runtimeconfig.Config
		wantErr string
	}{
		{
			name: "unix without auth",
			ep:   endpoint.Endpoint{Scheme: "unix", Address: "/tmp/cleanroom.sock"},
		},
		{
			name: "loopback http without auth",
			ep:   endpoint.Endpoint{Scheme: "http", Address: "http://127.0.0.1:7777"},
		},
		{
			name: "loopback https without auth",
			ep:   endpoint.Endpoint{Scheme: "https", Address: "https://localhost:7777"},
		},
		{
			name:    "non-loopback https without auth",
			ep:      endpoint.Endpoint{Scheme: "https", Address: "https://0.0.0.0:7777"},
			wantErr: "requires auth.required=true",
		},
		{
			name: "non-loopback https with auth",
			ep:   endpoint.Endpoint{Scheme: "https", Address: "https://0.0.0.0:7777"},
			cfg: runtimeconfig.Config{
				Auth: runtimeconfig.AuthConfig{Required: true},
			},
		},
		{
			name: "non-loopback http with auth",
			ep:   endpoint.Endpoint{Scheme: "http", Address: "http://0.0.0.0:7777"},
			cfg: runtimeconfig.Config{
				Auth: runtimeconfig.AuthConfig{Required: true},
			},
			wantErr: "non-loopback http listen endpoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateControlPlaneListenAuth(tt.ep, tt.cfg, "cleanroom serve")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateControlPlaneListenAuth returned error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected endpoint validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateBearerAuthClientEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ep      endpoint.Endpoint
		wantErr bool
	}{
		{
			name: "unix",
			ep:   endpoint.Endpoint{Scheme: "unix", Address: "/tmp/cleanroom.sock"},
		},
		{
			name: "loopback http",
			ep:   endpoint.Endpoint{Scheme: "http", Address: "http://127.0.0.1:7777"},
		},
		{
			name: "localhost http",
			ep:   endpoint.Endpoint{Scheme: "http", Address: "http://localhost:7777"},
		},
		{
			name: "remote https",
			ep:   endpoint.Endpoint{Scheme: "https", Address: "https://cleanroom.example.com:7777"},
		},
		{
			name:    "public http",
			ep:      endpoint.Endpoint{Scheme: "http", Address: "http://0.0.0.0:7777"},
			wantErr: true,
		},
		{
			name:    "remote http",
			ep:      endpoint.Endpoint{Scheme: "http", Address: "http://cleanroom.example.com:7777"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateBearerAuthClientEndpoint(tt.ep)
			if tt.wantErr && err == nil {
				t.Fatal("expected endpoint validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateBearerAuthClientEndpoint returned error: %v", err)
			}
		})
	}
}

func TestClientFlagsConnectRejectsBearerTokenForRemoteHTTP(t *testing.T) {
	t.Setenv("CLEANROOM_AUTH_TOKEN", "secret-token")

	_, err := (&clientFlags{Host: "http://cleanroom.example.com:7777"}).connect(&runtimeContext{})
	if err == nil {
		t.Fatal("expected connect to reject bearer token on remote http")
	}
	if !strings.Contains(err.Error(), "non-loopback http endpoint") {
		t.Fatalf("unexpected error: %v", err)
	}
}

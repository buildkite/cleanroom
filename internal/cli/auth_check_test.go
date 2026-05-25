package cli

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/golang-jwt/jwt/v5"
)

func TestAuthCheckCommandReportsAllowedDecision(t *testing.T) {
	fixture := newAuthCheckFixture(t)

	stdout, readStdout := makeStdoutCapture(t)
	err := (&AuthCheckCommand{
		Config:    fixture.configPath,
		TokenFile: fixture.tokenPath,
		Action:    "sandbox.create",
		Request:   fixture.requestPath,
		JSON:      true,
	}).Run(&runtimeContext{CWD: fixture.dir, Stdout: stdout})
	if err != nil {
		t.Fatalf("AuthCheckCommand.Run returned error: %v", err)
	}

	var decision authz.Decision
	if err := json.Unmarshal([]byte(readStdout()), &decision); err != nil {
		t.Fatalf("decode auth check json: %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("expected allowed decision, got %#v", decision)
	}
	if got, want := decision.Principal.ID, "oidc:github-actions:repo:buildkite/cleanroom:ref:refs/heads/main"; got != want {
		t.Fatalf("unexpected principal ID: got %q want %q", got, want)
	}
	if got, want := decision.Binding, "cleanroom-repo-bots"; got != want {
		t.Fatalf("unexpected binding: got %q want %q", got, want)
	}
}

func TestAuthCheckCommandReportsDeniedDecision(t *testing.T) {
	fixture := newAuthCheckFixture(t)
	denyRequestPath := filepath.Join(fixture.dir, "deny-request.json")
	if err := os.WriteFile(denyRequestPath, []byte(`{
  "repository": {"remote_url": "https://github.com/buildkite/other.git"},
  "backend": "darwin-vz",
  "policy": {
    "resources": {"vcpus": 4, "memory_bytes": 8589934592},
    "docker": {"required": false},
    "network_default": "deny"
  }
}`), 0o644); err != nil {
		t.Fatalf("write deny request: %v", err)
	}

	stdout, readStdout := makeStdoutCapture(t)
	err := (&AuthCheckCommand{
		Config:    fixture.configPath,
		TokenFile: fixture.tokenPath,
		Action:    "sandbox.create",
		Request:   denyRequestPath,
		JSON:      true,
	}).Run(&runtimeContext{CWD: fixture.dir, Stdout: stdout})
	var exitErr hasExitCode
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected denied exit code 1, got %v", err)
	}

	var decision authz.Decision
	if err := json.Unmarshal([]byte(readStdout()), &decision); err != nil {
		t.Fatalf("decode auth check json: %v", err)
	}
	if decision.Allowed {
		t.Fatalf("expected denied decision, got %#v", decision)
	}
	if got, want := decision.Reason, authz.ReasonConditionFalse; got != want {
		t.Fatalf("unexpected deny reason: got %q want %q", got, want)
	}
}

func TestRunAuthCheckBypassesBrokenDefaultRuntimeConfig(t *testing.T) {
	fixture := newAuthCheckFixture(t)
	xdgConfigHome := filepath.Join(fixture.dir, "xdg")
	defaultConfigPath := filepath.Join(xdgConfigHome, "cleanroom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(defaultConfigPath), 0o755); err != nil {
		t.Fatalf("mkdir default config dir: %v", err)
	}
	if err := os.WriteFile(defaultConfigPath, []byte("default_backend: broken\n"), 0o644); err != nil {
		t.Fatalf("write broken config: %v", err)
	}

	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	if err := Run([]string{"auth", "check", "--config", fixture.configPath, "--token-file", fixture.tokenPath, "--action", "sandbox.create", "--request", fixture.requestPath, "--json"}, "dev"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

type authCheckFixture struct {
	dir         string
	configPath  string
	policyPath  string
	tokenPath   string
	requestPath string
}

func newAuthCheckFixture(t *testing.T) authCheckFixture {
	t.Helper()
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	key := mustCLIAuthRSAKey(t)
	jwks := cliAuthJWKSServer(t, "kid-1", key)

	policyPath := filepath.Join(dir, "auth-policy.yaml")
	if err := os.WriteFile(policyPath, []byte(cliAuthPolicyYAML()), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	config := `default_backend: firecracker
auth:
  required: true
  oidc:
    issuers:
      - name: github-actions
        issuer: https://token.actions.githubusercontent.com
        audiences: [cleanroom]
        jwks_url: ` + jwks.URL + `
        clock_skew_seconds: 60
        max_token_lifetime_seconds: 3600
  policy_file: auth-policy.yaml
`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	tokenPath := filepath.Join(dir, "token.jwt")
	token := cliAuthSignToken(t, key, "kid-1", jwt.MapClaims{
		"iss":        "https://token.actions.githubusercontent.com",
		"sub":        "repo:buildkite/cleanroom:ref:refs/heads/main",
		"aud":        []string{"cleanroom"},
		"iat":        jwt.NewNumericDate(now.Add(-time.Minute)),
		"nbf":        jwt.NewNumericDate(now.Add(-time.Minute)),
		"exp":        jwt.NewNumericDate(now.Add(time.Minute)),
		"repository": "buildkite/cleanroom",
	})
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, []byte(`{
  "repository": {"remote_url": "https://github.com/buildkite/cleanroom.git"},
  "backend": "darwin-vz",
  "policy": {
    "resources": {"vcpus": 4, "memory_bytes": 8589934592},
    "docker": {"required": false},
    "network_default": "deny"
  }
}`), 0o644); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return authCheckFixture{
		dir:         dir,
		configPath:  configPath,
		policyPath:  policyPath,
		tokenPath:   tokenPath,
		requestPath: requestPath,
	}
}

func mustCLIAuthRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func cliAuthJWKSServer(t *testing.T, kid string, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	payload := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": kid,
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
			},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Fatalf("encode jwks: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func cliAuthSignToken(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func cliAuthPolicyYAML() string {
	return `bindings:
  - name: cleanroom-repo-bots
    when: >
      token.issuer == "github-actions" &&
      claims.repository == "buildkite/cleanroom"
    principal:
      id: 'oidc:${token.issuer}:${claims.sub}'
      scope: 'repo:${claims.repository}'
    grants:
      - name: create-cleanroom-sandbox
        actions: [sandbox.create]
        resources: [sandbox]
        condition: >
          request.repository.remote_url == "https://github.com/buildkite/cleanroom.git" &&
          request.backend in ["darwin-vz"] &&
          request.policy.resources.vcpus <= 4 &&
          request.policy.resources.memory_bytes <= 8589934592 &&
          request.policy.docker.required == false &&
          request.policy.network_default == "deny"
`
}

func TestResourceKindForAction(t *testing.T) {
	tests := map[string]string{
		"sandbox.file.read": "sandbox",
		"execution.stream":  "execution",
		"snapshot.restore":  "snapshot",
		"cache_peer.lookup": "cache_peer",
	}
	for action, want := range tests {
		if got := resourceKindForAction(action, ""); got != want {
			t.Fatalf("resourceKindForAction(%q) = %q, want %q", action, got, want)
		}
	}
	if got := resourceKindForAction("sandbox.create", "snapshot"); got != "snapshot" {
		t.Fatalf("explicit resource should win, got %q", got)
	}
}

func TestResolveAuthPolicyPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	got, err := resolveAuthPolicyPath("/work", configPath, "", "auth-policy.yaml")
	if err != nil {
		t.Fatalf("resolveAuthPolicyPath returned error: %v", err)
	}
	if want := filepath.Join(dir, "auth-policy.yaml"); got != want {
		t.Fatalf("unexpected policy path: got %q want %q", got, want)
	}
	got, err = resolveAuthPolicyPath("/work", configPath, "explicit.yaml", "auth-policy.yaml")
	if err != nil {
		t.Fatalf("resolve explicit path returned error: %v", err)
	}
	if want := filepath.Join("/work", "explicit.yaml"); got != want {
		t.Fatalf("unexpected explicit policy path: got %q want %q", got, want)
	}
	_, err = resolveAuthPolicyPath("/work", configPath, "", " ")
	if err == nil || !strings.Contains(err.Error(), "auth policy file is required") {
		t.Fatalf("expected missing policy error, got %v", err)
	}
}

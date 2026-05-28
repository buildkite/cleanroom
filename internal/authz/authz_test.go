package authz

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/authconfig"
	"github.com/golang-jwt/jwt/v5"
)

func TestOIDCValidatorAcceptsValidToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := mustRSAKey(t)
	jwks := jwksServer(t, "kid-1", key)
	validator := newTestOIDCValidator(t, jwks.URL, now)

	token := signTestToken(t, key, "kid-1", jwt.MapClaims{
		"iss":        "https://issuer.example",
		"sub":        "bot-123",
		"aud":        []string{"cleanroom"},
		"iat":        jwt.NewNumericDate(now.Add(-time.Minute)),
		"nbf":        jwt.NewNumericDate(now.Add(-time.Minute)),
		"exp":        jwt.NewNumericDate(now.Add(time.Minute)),
		"repository": "buildkite/cleanroom",
	})
	validated, err := validator.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if got, want := validated.IssuerName, "issuer"; got != want {
		t.Fatalf("unexpected issuer name: got %q want %q", got, want)
	}
	if got, want := validated.Subject, "bot-123"; got != want {
		t.Fatalf("unexpected subject: got %q want %q", got, want)
	}
	if got, want := validated.Claims["repository"], "buildkite/cleanroom"; got != want {
		t.Fatalf("unexpected repository claim: got %v want %v", got, want)
	}
}

func TestOIDCValidatorEnforcesRequiredClaims(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := mustRSAKey(t)
	jwks := jwksServer(t, "kid-1", key)
	validator, err := NewOIDCValidator([]authconfig.OIDCIssuerConfig{
		{
			Name:                    "issuer",
			Issuer:                  "https://issuer.example",
			Audiences:               []string{"cleanroom"},
			JWKSURL:                 jwks.URL,
			AllowedAlgorithms:       []string{"RS256"},
			ClockSkewSeconds:        60,
			MaxTokenLifetimeSeconds: 3600,
			RequiredClaims:          map[string]string{"organization_id": "org_123"},
		},
	}, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewOIDCValidator returned error: %v", err)
	}
	baseClaims := jwt.MapClaims{
		"iss": "https://issuer.example",
		"sub": "bot-123",
		"aud": []string{"cleanroom"},
		"iat": jwt.NewNumericDate(now.Add(-time.Minute)),
		"nbf": jwt.NewNumericDate(now.Add(-time.Minute)),
		"exp": jwt.NewNumericDate(now.Add(time.Minute)),
	}
	if _, err := validator.Validate(context.Background(), signTestToken(t, key, "kid-1", baseClaims)); err == nil || !strings.Contains(err.Error(), `required claim "organization_id" is missing`) {
		t.Fatalf("expected missing required claim error, got %v", err)
	}
	if _, err := validator.Validate(context.Background(), signTestToken(t, key, "kid-1", withClaim(baseClaims, "organization_id", "other"))); err == nil || !strings.Contains(err.Error(), `required claim "organization_id" does not match`) {
		t.Fatalf("expected required claim mismatch error, got %v", err)
	}
	if _, err := validator.Validate(context.Background(), signTestToken(t, key, "kid-1", withClaim(baseClaims, "organization_id", "org_123"))); err != nil {
		t.Fatalf("Validate with required claim returned error: %v", err)
	}
}

func TestOIDCValidatorRejectsDuplicateNormalizedRequiredClaims(t *testing.T) {
	_, err := NewOIDCValidator([]authconfig.OIDCIssuerConfig{
		{
			Name:                    "issuer",
			Issuer:                  "https://issuer.example",
			Audiences:               []string{"cleanroom"},
			JWKSURL:                 "https://issuer.example/jwks",
			AllowedAlgorithms:       []string{"RS256"},
			ClockSkewSeconds:        60,
			MaxTokenLifetimeSeconds: 3600,
			RequiredClaims: map[string]string{
				"repository_owner_id":  "123456",
				" repository_owner_id": "654321",
			},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate required claim error")
	}
	if got, want := err.Error(), `issuer[0].required_claims contains duplicate claim name "repository_owner_id" after trimming whitespace`; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %v", want, err)
	}
}

func TestOIDCValidatorRefreshesJWKSOnReusedKidSignatureFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	firstKey := mustRSAKey(t)
	secondKey := mustRSAKey(t)
	var mu sync.Mutex
	currentKey := firstKey
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		key := currentKey
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				jwkPayload("kid-1", &key.PublicKey),
			},
		}); err != nil {
			t.Fatalf("encode jwks: %v", err)
		}
	}))
	t.Cleanup(jwks.Close)
	validator := newTestOIDCValidator(t, jwks.URL, now)
	claims := jwt.MapClaims{
		"iss": "https://issuer.example",
		"sub": "bot-123",
		"aud": []string{"cleanroom"},
		"iat": jwt.NewNumericDate(now.Add(-time.Minute)),
		"nbf": jwt.NewNumericDate(now.Add(-time.Minute)),
		"exp": jwt.NewNumericDate(now.Add(time.Minute)),
	}
	if _, err := validator.Validate(context.Background(), signTestToken(t, firstKey, "kid-1", claims)); err != nil {
		t.Fatalf("Validate with first key returned error: %v", err)
	}

	mu.Lock()
	currentKey = secondKey
	mu.Unlock()
	if _, err := validator.Validate(context.Background(), signTestToken(t, secondKey, "kid-1", claims)); err != nil {
		t.Fatalf("Validate after key rotation returned error: %v", err)
	}
}

func TestOIDCValidatorIgnoresUnsupportedJWKSKeys(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := mustRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{"kty": "oct", "kid": "symmetric", "k": "secret"},
				jwkPayload("kid-1", &key.PublicKey),
			},
		}); err != nil {
			t.Fatalf("encode jwks: %v", err)
		}
	}))
	t.Cleanup(jwks.Close)
	validator := newTestOIDCValidator(t, jwks.URL, now)
	token := signTestToken(t, key, "kid-1", jwt.MapClaims{
		"iss": "https://issuer.example",
		"sub": "bot-123",
		"aud": []string{"cleanroom"},
		"iat": jwt.NewNumericDate(now.Add(-time.Minute)),
		"nbf": jwt.NewNumericDate(now.Add(-time.Minute)),
		"exp": jwt.NewNumericDate(now.Add(time.Minute)),
	})
	if _, err := validator.Validate(context.Background(), token); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestOIDCValidatorExpiresJWKSCache(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	firstKey := mustRSAKey(t)
	secondKey := mustRSAKey(t)
	var mu sync.Mutex
	currentKid := "kid-1"
	currentKey := firstKey
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		kid := currentKid
		key := currentKey
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				jwkPayload(kid, &key.PublicKey),
			},
		}); err != nil {
			t.Fatalf("encode jwks: %v", err)
		}
	}))
	t.Cleanup(jwks.Close)
	validator, err := NewOIDCValidator([]authconfig.OIDCIssuerConfig{
		{
			Name:                    "issuer",
			Issuer:                  "https://issuer.example",
			Audiences:               []string{"cleanroom"},
			JWKSURL:                 jwks.URL,
			AllowedAlgorithms:       []string{"RS256"},
			ClockSkewSeconds:        60,
			MaxTokenLifetimeSeconds: 3600,
		},
	}, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewOIDCValidator returned error: %v", err)
	}
	claims := jwt.MapClaims{
		"iss": "https://issuer.example",
		"sub": "bot-123",
		"aud": []string{"cleanroom"},
		"iat": jwt.NewNumericDate(now.Add(-time.Minute)),
		"nbf": jwt.NewNumericDate(now.Add(-time.Minute)),
		"exp": jwt.NewNumericDate(now.Add(10 * time.Minute)),
	}
	oldToken := signTestToken(t, firstKey, "kid-1", claims)
	if _, err := validator.Validate(context.Background(), oldToken); err != nil {
		t.Fatalf("Validate with first key returned error: %v", err)
	}

	mu.Lock()
	currentKid = "kid-2"
	currentKey = secondKey
	mu.Unlock()
	now = now.Add(defaultJWKSCacheMaxAge + time.Second)
	if _, err := validator.Validate(context.Background(), oldToken); err == nil {
		t.Fatal("expected old token to fail after JWKS cache expiry")
	}
}

func TestOIDCValidatorRejectsInvalidTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	trustedKey := mustRSAKey(t)
	jwks := jwksServer(t, "kid-1", trustedKey)
	validator := newTestOIDCValidator(t, jwks.URL, now)
	otherKey := mustRSAKey(t)

	baseClaims := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": "https://issuer.example",
			"sub": "bot-123",
			"aud": []string{"cleanroom"},
			"iat": jwt.NewNumericDate(now.Add(-time.Minute)),
			"nbf": jwt.NewNumericDate(now.Add(-time.Minute)),
			"exp": jwt.NewNumericDate(now.Add(time.Minute)),
		}
	}

	tests := []struct {
		name    string
		token   string
		wantErr string
	}{
		{
			name:    "issuer",
			token:   signTestToken(t, trustedKey, "kid-1", withClaim(baseClaims(), "iss", "https://evil.example")),
			wantErr: "untrusted issuer",
		},
		{
			name:    "audience",
			token:   signTestToken(t, trustedKey, "kid-1", withClaim(baseClaims(), "aud", []string{"other"})),
			wantErr: "aud",
		},
		{
			name:    "signature",
			token:   signTestToken(t, otherKey, "kid-1", baseClaims()),
			wantErr: "signature",
		},
		{
			name:    "expiry",
			token:   signTestToken(t, trustedKey, "kid-1", withClaim(baseClaims(), "exp", jwt.NewNumericDate(now.Add(-2*time.Minute)))),
			wantErr: "expired",
		},
		{
			name:    "missing kid",
			token:   signTestToken(t, trustedKey, "", baseClaims()),
			wantErr: "kid header is required",
		},
		{
			name:    "unsupported algorithm",
			token:   signHS256TestToken(t, "kid-1", baseClaims()),
			wantErr: "signing method",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := validator.Validate(context.Background(), tt.token)
			if err == nil {
				t.Fatal("expected token validation error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantErr)) {
				t.Fatalf("expected error to contain %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestPolicyBindsPrincipalAndAuthorizesGrant(t *testing.T) {
	policy := compileTestPolicy(t, testPolicyYAML())
	token := ValidatedToken{
		IssuerName: "github-actions",
		Issuer:     "https://token.actions.githubusercontent.com",
		Subject:    "repo:buildkite/cleanroom:ref:refs/heads/main",
		Claims: map[string]any{
			"sub":        "repo:buildkite/cleanroom:ref:refs/heads/main",
			"repository": "buildkite/cleanroom",
		},
	}
	bound, err := policy.Bind(token)
	if err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	if got, want := bound.Principal.ID, "oidc:github-actions:repo:buildkite/cleanroom:ref:refs/heads/main"; got != want {
		t.Fatalf("unexpected principal ID: got %q want %q", got, want)
	}
	if got, want := bound.Principal.Scope, "repo:buildkite/cleanroom"; got != want {
		t.Fatalf("unexpected principal scope: got %q want %q", got, want)
	}

	decision := bound.Authorize(DecisionRequest{
		Action:   "sandbox.create",
		Resource: Resource{Kind: "sandbox"},
		Request: map[string]any{
			"repository": map[string]any{"remote_url": "https://github.com/buildkite/cleanroom.git"},
			"backend":    "darwin-vz",
			"policy": map[string]any{
				"resources":       map[string]any{"vcpus": int64(4), "memory_bytes": int64(8 << 30)},
				"docker":          map[string]any{"required": false},
				"network_default": "deny",
			},
		},
	})
	if !decision.Allowed {
		t.Fatalf("expected authorization to allow, got %#v", decision)
	}
	if got, want := decision.Binding, "cleanroom-repo-bots"; got != want {
		t.Fatalf("unexpected binding: got %q want %q", got, want)
	}
	if got, want := decision.Grant, "create-cleanroom-sandbox"; got != want {
		t.Fatalf("unexpected grant: got %q want %q", got, want)
	}
}

func TestPolicyFailsClosedBindingEvaluationAfterWhenError(t *testing.T) {
	policy := compileTestPolicy(t, `bindings:
  - name: missing-claim
    when: 'claims.repository.owner == "buildkite"'
    principal:
      id: 'bad:${claims.repository}'
    grants:
      - actions: [sandbox.create]
        resources: [sandbox]
  - name: fallback
    when: 'token.issuer == "github-actions"'
    principal:
      id: 'oidc:${token.issuer}:${claims.sub}'
    grants:
      - actions: [sandbox.create]
        resources: [sandbox]
`)
	bound, err := policy.Bind(ValidatedToken{
		IssuerName: "github-actions",
		Subject:    "repo:buildkite/cleanroom:ref:refs/heads/main",
		Claims: map[string]any{
			"sub": "repo:buildkite/cleanroom:ref:refs/heads/main",
		},
	})
	if err == nil {
		t.Fatal("expected binding condition error")
	}
	if bound.Principal.ID != "" {
		t.Fatalf("expected no bound principal, got %#v", bound.Principal)
	}
	decision, ok := DecisionFromError(err)
	if !ok {
		t.Fatalf("expected decision error, got %v", err)
	}
	if got, want := decision.Reason, ReasonConditionError; got != want {
		t.Fatalf("unexpected deny reason: got %q want %q", got, want)
	}
	if got, want := decision.Binding, "missing-claim"; got != want {
		t.Fatalf("unexpected binding: got %q want %q", got, want)
	}
}

func TestPolicyContinuesGrantEvaluationAfterConditionError(t *testing.T) {
	policy := compileTestPolicy(t, `bindings:
  - name: cleanroom-repo-bots
    when: 'token.issuer == "github-actions"'
    principal:
      id: 'oidc:${token.issuer}:${claims.sub}'
    grants:
      - name: optional-request-field
        actions: [sandbox.create]
        resources: [sandbox]
        condition: 'request.repository.remote_url == "https://github.com/buildkite/cleanroom.git"'
      - name: fallback
        actions: [sandbox.create]
        resources: [sandbox]
`)
	bound, err := policy.Bind(ValidatedToken{
		IssuerName: "github-actions",
		Subject:    "repo:buildkite/cleanroom:ref:refs/heads/main",
		Claims: map[string]any{
			"sub": "repo:buildkite/cleanroom:ref:refs/heads/main",
		},
	})
	if err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	decision := bound.Authorize(DecisionRequest{
		Action:   "sandbox.create",
		Resource: Resource{Kind: "sandbox"},
		Request:  map[string]any{},
	})
	if !decision.Allowed {
		t.Fatalf("expected fallback grant to allow, got %#v", decision)
	}
	if got, want := decision.Grant, "fallback"; got != want {
		t.Fatalf("unexpected grant: got %q want %q", got, want)
	}
}

func TestPolicyCompilesCELComprehensionVariables(t *testing.T) {
	policy := compileTestPolicy(t, `bindings:
  - name: cleanroom-repo-bots
    when: 'token.issuer == "github-actions"'
    principal:
      id: 'oidc:${token.issuer}:${claims.sub}'
    grants:
      - name: private-hosts
        actions: [sandbox.create]
        resources: [sandbox]
        condition: 'request.policy.network.hosts.all(host, host == "github.com")'
`)
	bound, err := policy.Bind(ValidatedToken{
		IssuerName: "github-actions",
		Subject:    "repo:buildkite/cleanroom:ref:refs/heads/main",
		Claims: map[string]any{
			"sub": "repo:buildkite/cleanroom:ref:refs/heads/main",
		},
	})
	if err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	decision := bound.Authorize(DecisionRequest{
		Action:   "sandbox.create",
		Resource: Resource{Kind: "sandbox"},
		Request: map[string]any{
			"policy": map[string]any{
				"network": map[string]any{
					"hosts": []any{"github.com"},
				},
			},
		},
	})
	if !decision.Allowed {
		t.Fatalf("expected comprehension grant to allow, got %#v", decision)
	}
}

func TestPolicyRejectsMalformedBindingsAndUnknownCELFields(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name: "missing principal id",
			raw: `bindings:
  - name: bad
    when: 'token.issuer == "github-actions"'
    principal:
      scope: repo
    grants:
      - actions: [sandbox.create]
        resources: [sandbox]
`,
			wantErr: "principal.id is required",
		},
		{
			name: "unknown binding field",
			raw: `bindings:
  - name: bad
    when: 'token.missing == "github-actions"'
    principal:
      id: 'oidc:${token.issuer}:${claims.sub}'
    grants:
      - actions: [sandbox.create]
        resources: [sandbox]
`,
			wantErr: `unknown CEL field "token.missing"`,
		},
		{
			name: "unknown grant field",
			raw: `bindings:
  - name: bad
    when: 'token.issuer == "github-actions"'
    principal:
      id: 'oidc:${token.issuer}:${claims.sub}'
    grants:
      - actions: [sandbox.create]
        resources: [sandbox]
        condition: 'request.nope == "x"'
`,
			wantErr: `unknown CEL field "request.nope"`,
		},
		{
			name: "wildcard action",
			raw: `bindings:
  - name: bad
    when: 'token.issuer == "github-actions"'
    principal:
      id: 'oidc:${token.issuer}:${claims.sub}'
    grants:
      - actions: ["*"]
        resources: [sandbox]
`,
			wantErr: `unknown action "*"`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePolicy([]byte(tt.raw))
			if err == nil {
				t.Fatal("expected policy parse error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error to contain %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestPolicyDeniesConditionFailures(t *testing.T) {
	policy := compileTestPolicy(t, testPolicyYAML())
	bound, err := policy.Bind(ValidatedToken{
		IssuerName: "github-actions",
		Subject:    "repo:buildkite/cleanroom:ref:refs/heads/main",
		Claims: map[string]any{
			"sub":        "repo:buildkite/cleanroom:ref:refs/heads/main",
			"repository": "buildkite/cleanroom",
		},
	})
	if err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	decision := bound.Authorize(DecisionRequest{
		Action:   "sandbox.create",
		Resource: Resource{Kind: "sandbox"},
		Request: map[string]any{
			"repository": map[string]any{"remote_url": "https://github.com/buildkite/other.git"},
			"backend":    "darwin-vz",
			"policy": map[string]any{
				"resources":       map[string]any{"vcpus": int64(4), "memory_bytes": int64(8 << 30)},
				"docker":          map[string]any{"required": false},
				"network_default": "deny",
			},
		},
	})
	if decision.Allowed {
		t.Fatalf("expected authorization to deny, got %#v", decision)
	}
	if got, want := decision.Reason, ReasonConditionFalse; got != want {
		t.Fatalf("unexpected deny reason: got %q want %q", got, want)
	}
}

func TestPolicyRejectsStructuredTemplateClaims(t *testing.T) {
	policy := compileTestPolicy(t, `bindings:
  - name: structured-claim
    when: 'token.issuer == "github-actions"'
    principal:
      id: 'oidc:${token.issuer}:${claims.repository}'
    grants:
      - actions: [sandbox.create]
        resources: [sandbox]
`)
	_, err := policy.Bind(ValidatedToken{
		IssuerName: "github-actions",
		Subject:    "repo:buildkite/cleanroom:ref:refs/heads/main",
		Claims: map[string]any{
			"repository": map[string]any{"name": "buildkite/cleanroom"},
		},
	})
	if err == nil {
		t.Fatal("expected structured template claim error")
	}
	if !strings.Contains(err.Error(), "must resolve to a scalar") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPolicyRejectsEmptyRenderedPrincipalID(t *testing.T) {
	policy := compileTestPolicy(t, `bindings:
  - name: empty-principal
    when: 'token.issuer == "github-actions"'
    principal:
      id: '${claims.cleanroom_principal}'
    grants:
      - actions: [sandbox.create]
        resources: [sandbox]
`)
	_, err := policy.Bind(ValidatedToken{
		IssuerName: "github-actions",
		Subject:    "repo:buildkite/cleanroom:ref:refs/heads/main",
		Claims: map[string]any{
			"cleanroom_principal": " ",
		},
	})
	if err == nil {
		t.Fatal("expected empty principal id error")
	}
	if !strings.Contains(err.Error(), "principal.id rendered empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPolicyReportsNoBinding(t *testing.T) {
	policy := compileTestPolicy(t, testPolicyYAML())
	_, err := policy.Bind(ValidatedToken{
		IssuerName: "github-actions",
		Subject:    "repo:buildkite/other:ref:refs/heads/main",
		Claims: map[string]any{
			"sub":        "repo:buildkite/other:ref:refs/heads/main",
			"repository": "buildkite/other",
		},
	})
	if !errors.Is(err, ErrNoBinding) {
		t.Fatalf("expected ErrNoBinding, got %v", err)
	}
}

func newTestOIDCValidator(t *testing.T, jwksURL string, now time.Time) *OIDCValidator {
	t.Helper()
	validator, err := NewOIDCValidator([]authconfig.OIDCIssuerConfig{
		{
			Name:                    "issuer",
			Issuer:                  "https://issuer.example",
			Audiences:               []string{"cleanroom"},
			JWKSURL:                 jwksURL,
			AllowedAlgorithms:       []string{"RS256"},
			ClockSkewSeconds:        60,
			MaxTokenLifetimeSeconds: 3600,
		},
	}, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewOIDCValidator returned error: %v", err)
	}
	return validator
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func jwksServer(t *testing.T, kid string, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	payload := map[string]any{
		"keys": []map[string]any{
			jwkPayload(kid, &key.PublicKey),
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

func jwkPayload(kid string, key *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func signTestToken(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func signHS256TestToken(t *testing.T, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString([]byte("secret"))
	if err != nil {
		t.Fatalf("sign HS256 token: %v", err)
	}
	return signed
}

func withClaim(claims jwt.MapClaims, key string, value any) jwt.MapClaims {
	claims[key] = value
	return claims
}

func compileTestPolicy(t *testing.T, raw string) *CompiledPolicy {
	t.Helper()
	policy, err := ParsePolicy([]byte(raw))
	if err != nil {
		t.Fatalf("ParsePolicy returned error: %v", err)
	}
	return policy
}

func testPolicyYAML() string {
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
        actions:
          - sandbox.create
        resources:
          - sandbox
        condition: >
          request.repository.remote_url == "https://github.com/buildkite/cleanroom.git" &&
          request.backend in ["darwin-vz"] &&
          request.policy.resources.vcpus <= 4 &&
          request.policy.resources.memory_bytes <= 8589934592 &&
          request.policy.docker.required == false &&
          request.policy.network_default == "deny"
`
}

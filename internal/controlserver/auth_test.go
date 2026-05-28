package controlserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type fakeTokenValidator struct {
	tokens map[string]authz.ValidatedToken
}

func (v fakeTokenValidator) Validate(_ context.Context, token string) (authz.ValidatedToken, error) {
	validated, ok := v.tokens[token]
	if !ok {
		return authz.ValidatedToken{}, errors.New("unknown token")
	}
	return validated, nil
}

func TestBearerAuthenticatorRejectsMissingToken(t *testing.T) {
	policy := testServerAuthPolicy(t)
	auth := BearerAuthenticator{
		Validator: fakeTokenValidator{tokens: map[string]authz.ValidatedToken{}},
		Policy:    policy,
	}

	if _, err := auth.Authenticate(context.Background(), ""); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("Authenticate missing token code = %v, want unauthenticated (err=%v)", connect.CodeOf(err), err)
	}
}

func TestBearerAuthenticatorRedactsInvalidToken(t *testing.T) {
	var logs bytes.Buffer
	logger, err := observability.NewLogger(&logs, "info", runtimeconfig.ObservabilityConfig{
		Logs: runtimeconfig.LogConfig{Format: "json"},
	})
	if err != nil {
		t.Fatalf("NewLogger returned error: %v", err)
	}
	policy := testServerAuthPolicy(t)
	auth := BearerAuthenticator{
		Validator: leakingTokenValidator{},
		Policy:    policy,
		Logger:    logger,
	}

	secret := "super-secret-token"
	_, err = auth.Authenticate(context.Background(), "Bearer "+secret)
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("Authenticate invalid token code = %v, want unauthenticated (err=%v)", connect.CodeOf(err), err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid token error leaked token: %v", err)
	}
	if !strings.Contains(err.Error(), authz.ReasonInvalidToken) {
		t.Fatalf("invalid token error missing reason code %q: %v", authz.ReasonInvalidToken, err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("invalid token audit log leaked token: %s", logs.String())
	}
	payload := findControlServerAuthDeniedLog(t, logs.String())
	if got, want := payload[observability.LogFieldReasonCode], authz.ReasonInvalidToken; got != want {
		t.Fatalf("unexpected auth log reason code: got %#v want %#v", got, want)
	}
}

func TestBearerAuthenticatorReportsBindingConditionError(t *testing.T) {
	policy, err := authz.CompilePolicy(authz.Policy{Bindings: []authz.Binding{
		{
			Name: "broken-binding",
			When: `claims.repository.owner == "buildkite"`,
			Principal: authz.PrincipalTemplate{
				ID: "oidc:${token.issuer}:${token.subject}",
			},
			Grants: []authz.Grant{{
				Actions:   []string{"sandbox.create"},
				Resources: []string{"sandbox"},
			}},
		},
		{
			Name: "fallback",
			When: `token.issuer == "test"`,
			Principal: authz.PrincipalTemplate{
				ID: "oidc:${token.issuer}:${token.subject}",
			},
			Grants: []authz.Grant{{
				Actions:   []string{"sandbox.create"},
				Resources: []string{"sandbox"},
			}},
		},
	}})
	if err != nil {
		t.Fatalf("CompilePolicy returned error: %v", err)
	}
	auth := BearerAuthenticator{
		Validator: fakeTokenValidator{tokens: map[string]authz.ValidatedToken{
			"alice-token": testValidatedToken("alice"),
		}},
		Policy: policy,
	}

	_, err = auth.Authenticate(context.Background(), "Bearer alice-token")
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Authenticate code = %v, want permission denied (err=%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), authz.ReasonConditionError) {
		t.Fatalf("binding condition error missing reason code %q: %v", authz.ReasonConditionError, err)
	}
}

func TestBearerAuthenticatorBindsPrincipalIntoHandlers(t *testing.T) {
	policy := testServerAuthPolicy(t)
	service := &controlservice.Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": &handlerTestAdapter{},
		},
	}
	auth := BearerAuthenticator{
		Validator: fakeTokenValidator{tokens: map[string]authz.ValidatedToken{
			"alice-token": testValidatedToken("alice"),
		}},
		Policy: policy,
	}
	httpServer := httptest.NewServer(New(service, nil, auth.Interceptor()).Handler())
	defer httpServer.Close()
	client := cleanroomv1connect.NewSandboxServiceClient(http.DefaultClient, httpServer.URL)

	missingReq := connect.NewRequest(&cleanroomv1.CreateSandboxRequest{Backend: "firecracker", Policy: handlerTestPolicy()})
	if _, err := client.CreateSandbox(context.Background(), missingReq); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("CreateSandbox without token code = %v, want unauthenticated (err=%v)", connect.CodeOf(err), err)
	}

	req := connect.NewRequest(&cleanroomv1.CreateSandboxRequest{Backend: "firecracker", Policy: handlerTestPolicy()})
	req.Header().Set("Authorization", "Bearer alice-token")
	resp, err := client.CreateSandbox(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateSandbox with token returned error: %v", err)
	}
	if resp.Msg.GetSandbox().GetSandboxId() == "" {
		t.Fatal("expected created sandbox id")
	}
}

type leakingTokenValidator struct{}

func (leakingTokenValidator) Validate(_ context.Context, token string) (authz.ValidatedToken, error) {
	return authz.ValidatedToken{}, errors.New("token rejected: " + token)
}

func findControlServerAuthDeniedLog(t *testing.T, raw string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("decode auth log %q: %v", line, err)
		}
		if payload["msg"] == "control server auth denied" {
			return payload
		}
	}
	t.Fatalf("control server auth denied log not found in:\n%s", raw)
	return nil
}

func testServerAuthPolicy(t *testing.T) *authz.CompiledPolicy {
	t.Helper()
	policy, err := authz.CompilePolicy(authz.Policy{Bindings: []authz.Binding{{
		Name: "test",
		Principal: authz.PrincipalTemplate{
			ID:    "oidc:${token.issuer}:${token.subject}",
			Scope: "scope:${token.subject}",
		},
		Grants: []authz.Grant{{
			Name:      "sandbox",
			Actions:   []string{"sandbox.create"},
			Resources: []string{"sandbox"},
		}},
	}}})
	if err != nil {
		t.Fatalf("CompilePolicy returned error: %v", err)
	}
	return policy
}

func testValidatedToken(subject string) authz.ValidatedToken {
	return authz.ValidatedToken{
		IssuerName: "test",
		Issuer:     "https://issuer.example.test",
		Subject:    subject,
		Claims:     map[string]any{"sub": subject},
		ExpiresAt:  time.Now().Add(time.Hour),
		IssuedAt:   time.Now(),
	}
}

package controlserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
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

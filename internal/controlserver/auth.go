package controlserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/log/v2"
	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/observability"
)

type TokenValidator interface {
	Validate(context.Context, string) (authz.ValidatedToken, error)
}

type BearerAuthenticator struct {
	Validator TokenValidator
	Policy    *authz.CompiledPolicy
	Logger    *log.Logger
}

func (a BearerAuthenticator) Interceptor() connect.Interceptor {
	return bearerAuthInterceptor{authenticator: a}
}

func (a BearerAuthenticator) Authenticate(ctx context.Context, header string) (authz.BoundPrincipal, error) {
	token, err := bearerToken(header)
	if err != nil {
		return a.authFailure(ctx, connect.CodeUnauthenticated, authz.ReasonMissing, err.Error())
	}
	if a.Validator == nil {
		return a.authFailure(ctx, connect.CodeUnauthenticated, authz.ReasonPolicyError, "auth validator is not configured")
	}
	if a.Policy == nil {
		return a.authFailure(ctx, connect.CodeUnauthenticated, authz.ReasonPolicyError, "auth policy is not configured")
	}
	validated, err := a.Validator.Validate(ctx, token)
	if err != nil {
		return a.authFailure(ctx, connect.CodeUnauthenticated, authz.ReasonInvalidToken, "invalid bearer token")
	}
	bound, err := a.Policy.Bind(validated)
	if err != nil {
		return a.authFailure(ctx, connect.CodePermissionDenied, authErrorReason(err), "bearer token is not authorized")
	}
	return bound, nil
}

func (a BearerAuthenticator) authFailure(ctx context.Context, code connect.Code, reason, message string) (authz.BoundPrincipal, error) {
	a.logAuthFailure(ctx, code, reason)
	return authz.BoundPrincipal{}, authConnectError(code, reason, message)
}

func (a BearerAuthenticator) logAuthFailure(ctx context.Context, code connect.Code, reason string) {
	if a.Logger == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = authz.ReasonPolicyError
	}
	observability.WithTraceContext(a.Logger, ctx).Warn(
		"control server auth denied",
		observability.LogFieldComponent, "auth",
		observability.LogFieldSubsystem, "controlserver",
		observability.LogFieldReasonCode, reason,
		"connect_code", code.String(),
	)
}

func authConnectError(code connect.Code, reason, message string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = authz.ReasonPolicyError
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = reason
	}
	return connect.NewError(code, fmt.Errorf("%s: %s", reason, message))
}

func authErrorReason(err error) string {
	if decision, ok := authz.DecisionFromError(err); ok {
		if reason := strings.TrimSpace(decision.Reason); reason != "" {
			return reason
		}
	}
	if errors.Is(err, authz.ErrNoBinding) {
		return authz.ReasonNoBinding
	}
	return authz.ReasonPolicyError
}

type bearerAuthInterceptor struct {
	authenticator BearerAuthenticator
}

func (i bearerAuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if skipBearerAuth(req.Spec()) {
			return next(ctx, req)
		}
		bound, err := i.authenticator.Authenticate(ctx, req.Header().Get("Authorization"))
		if err != nil {
			return nil, err
		}
		return next(authz.ContextWithBoundPrincipal(ctx, bound), req)
	}
}

func (i bearerAuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i bearerAuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if skipBearerAuth(conn.Spec()) {
			return next(ctx, conn)
		}
		bound, err := i.authenticator.Authenticate(ctx, conn.RequestHeader().Get("Authorization"))
		if err != nil {
			return err
		}
		return next(authz.ContextWithBoundPrincipal(ctx, bound), conn)
	}
}

func skipBearerAuth(spec connect.Spec) bool {
	return strings.Contains(spec.Procedure, ".CachePeerService/")
}

func bearerToken(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", errors.New("missing bearer token")
	}
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", errors.New("authorization header must be Bearer")
	}
	return strings.TrimSpace(token), nil
}

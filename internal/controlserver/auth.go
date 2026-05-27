package controlserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/authz"
)

type TokenValidator interface {
	Validate(context.Context, string) (authz.ValidatedToken, error)
}

type BearerAuthenticator struct {
	Validator TokenValidator
	Policy    *authz.CompiledPolicy
}

func (a BearerAuthenticator) Interceptor() connect.Interceptor {
	return bearerAuthInterceptor{authenticator: a}
}

func (a BearerAuthenticator) Authenticate(ctx context.Context, header string) (authz.BoundPrincipal, error) {
	token, err := bearerToken(header)
	if err != nil {
		return authz.BoundPrincipal{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if a.Validator == nil {
		return authz.BoundPrincipal{}, connect.NewError(connect.CodeUnauthenticated, errors.New("auth validator is not configured"))
	}
	if a.Policy == nil {
		return authz.BoundPrincipal{}, connect.NewError(connect.CodeUnauthenticated, errors.New("auth policy is not configured"))
	}
	validated, err := a.Validator.Validate(ctx, token)
	if err != nil {
		return authz.BoundPrincipal{}, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid bearer token: %w", err))
	}
	bound, err := a.Policy.Bind(validated)
	if err != nil {
		return authz.BoundPrincipal{}, connect.NewError(connect.CodePermissionDenied, err)
	}
	return bound, nil
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

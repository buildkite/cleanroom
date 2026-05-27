package authz

import "context"

type boundPrincipalContextKey struct{}

// ContextWithBoundPrincipal attaches a server-derived principal to a request
// context after token validation and policy binding.
func ContextWithBoundPrincipal(ctx context.Context, principal BoundPrincipal) context.Context {
	return context.WithValue(ctx, boundPrincipalContextKey{}, principal)
}

func BoundPrincipalFromContext(ctx context.Context) (BoundPrincipal, bool) {
	if ctx == nil {
		return BoundPrincipal{}, false
	}
	principal, ok := ctx.Value(boundPrincipalContextKey{}).(BoundPrincipal)
	if !ok || principal.Principal.ID == "" {
		return BoundPrincipal{}, false
	}
	return principal, true
}

package backend

import (
	"context"
	"strings"
)

// ProvisionProgressFunc receives short user-facing provisioning progress messages.
type ProvisionProgressFunc func(message string)

type provisionProgressContextKey struct{}

// ContextWithProvisionProgress attaches request-scoped provisioning progress to ctx.
func ContextWithProvisionProgress(ctx context.Context, progress ProvisionProgressFunc) context.Context {
	if progress == nil {
		return ctx
	}
	return context.WithValue(ctx, provisionProgressContextKey{}, progress)
}

// ReportProvisionProgress emits a provisioning progress message when ctx carries a reporter.
func ReportProvisionProgress(ctx context.Context, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	progress, _ := ctx.Value(provisionProgressContextKey{}).(ProvisionProgressFunc)
	if progress == nil {
		return
	}
	progress(message)
}

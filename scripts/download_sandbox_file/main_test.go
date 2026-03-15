package main

import (
	"testing"
	"time"
)

func TestRequestContextAddsDeadlineWhenTimeoutPositive(t *testing.T) {
	t.Helper()

	const timeout = 200 * time.Millisecond
	ctx, cancel := requestContext(timeout)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("expected request context with timeout %s to set a deadline", timeout)
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		t.Fatalf("expected deadline to be in the future, got remaining=%s", remaining)
	}
	if remaining > timeout+200*time.Millisecond {
		t.Fatalf("expected deadline close to timeout %s, got remaining=%s", timeout, remaining)
	}
}

func TestRequestContextDisablesDeadlineWhenTimeoutZero(t *testing.T) {
	t.Helper()

	ctx, cancel := requestContext(0)
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Fatalf("expected timeout=0 to disable request deadline")
	}
}

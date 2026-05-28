package authz

import (
	"errors"
	"fmt"
	"strings"
)

// DecisionError carries the stable authorization decision that caused a
// request to fail without requiring callers to parse human-readable text.
type DecisionError struct {
	Decision Decision
	Cause    error
}

func NewDecisionError(decision Decision, cause error) error {
	decision.Allowed = false
	if strings.TrimSpace(decision.Reason) == "" {
		decision.Reason = ReasonNoGrant
	}
	return &DecisionError{Decision: decision, Cause: cause}
}

func (e *DecisionError) Error() string {
	if e == nil {
		return ""
	}
	reason := strings.TrimSpace(e.Decision.Reason)
	if reason == "" {
		reason = ReasonNoGrant
	}
	action := strings.TrimSpace(e.Decision.Action)
	resourceKind := strings.TrimSpace(e.Decision.Resource.Kind)
	resourceID := strings.TrimSpace(e.Decision.Resource.ID)
	switch {
	case action != "" && resourceKind != "" && resourceID != "":
		return fmt.Sprintf("%s for %s on %s %q", reason, action, resourceKind, resourceID)
	case action != "" && resourceKind != "":
		return fmt.Sprintf("%s for %s on %s", reason, action, resourceKind)
	case e.Decision.Binding != "":
		return fmt.Sprintf("%s in binding %q", reason, e.Decision.Binding)
	default:
		return reason
	}
}

func (e *DecisionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func DecisionFromError(err error) (Decision, bool) {
	var decisionErr *DecisionError
	if !errors.As(err, &decisionErr) || decisionErr == nil {
		return Decision{}, false
	}
	return decisionErr.Decision, true
}

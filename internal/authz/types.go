// Package authz validates caller identity and evaluates Cleanroom authorization
// grants.
package authz

import "time"

// Principal is the server-derived caller identity used for authorization.
type Principal struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`
	Scope   string `json:"scope"`
}

// ResourceOwner identifies the principal that created a resource.
type ResourceOwner struct {
	PrincipalID string `json:"principal_id,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// Resource describes the authorization target.
type Resource struct {
	Kind  string        `json:"kind"`
	ID    string        `json:"id,omitempty"`
	Owner ResourceOwner `json:"owner,omitempty"`
}

// ValidatedToken is the trusted token shape after OIDC validation.
type ValidatedToken struct {
	IssuerName string         `json:"issuer_name"`
	Issuer     string         `json:"issuer"`
	Subject    string         `json:"subject"`
	Claims     map[string]any `json:"claims"`
	ExpiresAt  time.Time      `json:"expires_at,omitempty"`
	IssuedAt   time.Time      `json:"issued_at,omitempty"`
}

// DecisionRequest is the normalized input to a grant decision.
type DecisionRequest struct {
	Principal Principal      `json:"principal"`
	Claims    map[string]any `json:"claims,omitempty"`
	Action    string         `json:"action"`
	Resource  Resource       `json:"resource"`
	Request   map[string]any `json:"request,omitempty"`
}

// Decision explains why an authorization request was allowed or denied.
type Decision struct {
	Allowed   bool      `json:"allowed"`
	Principal Principal `json:"principal"`
	Action    string    `json:"action"`
	Resource  Resource  `json:"resource"`
	Binding   string    `json:"binding,omitempty"`
	Grant     string    `json:"grant,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

const (
	ReasonAllowed        = "allowed"
	ReasonMissing        = "auth_missing"
	ReasonNoBinding      = "auth_no_binding"
	ReasonNoGrant        = "auth_no_grant"
	ReasonConditionFalse = "auth_condition_false"
	ReasonConditionError = "auth_condition_error"
	ReasonOwnerMismatch  = "auth_owner_mismatch"
	ReasonMissingOwner   = "auth_resource_missing_owner"
	ReasonInvalidToken   = "auth_invalid_token"
	ReasonPolicyError    = "auth_policy_error"
)

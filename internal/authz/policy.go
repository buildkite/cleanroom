package authz

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var ErrNoBinding = errors.New("no matching auth binding")

type Policy struct {
	Bindings []Binding `yaml:"bindings" json:"bindings"`
}

type Binding struct {
	Name      string            `yaml:"name" json:"name"`
	When      string            `yaml:"when,omitempty" json:"when,omitempty"`
	Principal PrincipalTemplate `yaml:"principal" json:"principal"`
	Grants    []Grant           `yaml:"grants" json:"grants"`
}

type PrincipalTemplate struct {
	ID    string `yaml:"id" json:"id"`
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty"`
}

type Grant struct {
	Name      string   `yaml:"name,omitempty" json:"name,omitempty"`
	Actions   []string `yaml:"actions" json:"actions"`
	Resources []string `yaml:"resources" json:"resources"`
	Condition string   `yaml:"condition,omitempty" json:"condition,omitempty"`
}

type CompiledPolicy struct {
	bindings []compiledBinding
}

type compiledBinding struct {
	name      string
	when      *compiledBoolExpression
	principal PrincipalTemplate
	grants    []compiledGrant
}

type compiledGrant struct {
	name      string
	actions   map[string]struct{}
	resources map[string]struct{}
	condition *compiledBoolExpression
}

type BoundPrincipal struct {
	Principal Principal
	Claims    map[string]any
	Binding   string

	binding *compiledBinding
}

func LoadPolicyFile(path string) (*CompiledPolicy, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("auth policy path is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth policy %s: %w", path, err)
	}
	policy, err := ParsePolicy(raw)
	if err != nil {
		return nil, fmt.Errorf("parse auth policy %s: %w", path, err)
	}
	return policy, nil
}

func ParsePolicy(raw []byte) (*CompiledPolicy, error) {
	spec := Policy{}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}
	return CompilePolicy(spec)
}

func CompilePolicy(spec Policy) (*CompiledPolicy, error) {
	if len(spec.Bindings) == 0 {
		return nil, errors.New("auth policy must contain at least one binding")
	}
	compiled := &CompiledPolicy{
		bindings: make([]compiledBinding, 0, len(spec.Bindings)),
	}
	seenBindings := map[string]struct{}{}
	for i, binding := range spec.Bindings {
		name := strings.TrimSpace(binding.Name)
		if name == "" {
			return nil, fmt.Errorf("bindings[%d].name is required", i)
		}
		if _, ok := seenBindings[name]; ok {
			return nil, fmt.Errorf("duplicate bindings[%d].name %q", i, name)
		}
		seenBindings[name] = struct{}{}
		if strings.TrimSpace(binding.Principal.ID) == "" {
			return nil, fmt.Errorf("bindings[%d].principal.id is required", i)
		}
		when, err := compileBoolExpression(binding.When, bindingCELRules())
		if err != nil {
			return nil, fmt.Errorf("bindings[%d].when: %w", i, err)
		}
		grants, err := compileGrants(i, binding.Grants)
		if err != nil {
			return nil, err
		}
		compiled.bindings = append(compiled.bindings, compiledBinding{
			name: name,
			when: when,
			principal: PrincipalTemplate{
				ID:    strings.TrimSpace(binding.Principal.ID),
				Scope: strings.TrimSpace(binding.Principal.Scope),
			},
			grants: grants,
		})
	}
	return compiled, nil
}

func compileGrants(bindingIndex int, grants []Grant) ([]compiledGrant, error) {
	if len(grants) == 0 {
		return nil, fmt.Errorf("bindings[%d].grants must contain at least one grant", bindingIndex)
	}
	compiled := make([]compiledGrant, 0, len(grants))
	for i, grant := range grants {
		actions, err := normalizeActionSet(grant.Actions)
		if err != nil {
			return nil, fmt.Errorf("bindings[%d].grants[%d].actions: %w", bindingIndex, i, err)
		}
		resources, err := normalizeResourceSet(grant.Resources)
		if err != nil {
			return nil, fmt.Errorf("bindings[%d].grants[%d].resources: %w", bindingIndex, i, err)
		}
		condition, err := compileBoolExpression(grant.Condition, grantCELRules())
		if err != nil {
			return nil, fmt.Errorf("bindings[%d].grants[%d].condition: %w", bindingIndex, i, err)
		}
		name := strings.TrimSpace(grant.Name)
		if name == "" {
			name = fmt.Sprintf("grant[%d]", i)
		}
		compiled = append(compiled, compiledGrant{
			name:      name,
			actions:   actions,
			resources: resources,
			condition: condition,
		})
	}
	return compiled, nil
}

func (p *CompiledPolicy) Bind(token ValidatedToken) (BoundPrincipal, error) {
	if p == nil {
		return BoundPrincipal{}, errors.New("auth policy is nil")
	}
	tokenVars := map[string]any{
		"issuer":  token.IssuerName,
		"subject": token.Subject,
	}
	activation := map[string]any{
		"token":  tokenVars,
		"claims": token.Claims,
	}
	for i := range p.bindings {
		binding := &p.bindings[i]
		matched, err := binding.when.eval(activation)
		if err != nil {
			continue
		}
		if !matched {
			continue
		}
		principalID, err := renderPrincipalTemplate(binding.principal.ID, token, tokenVars)
		if err != nil {
			return BoundPrincipal{}, fmt.Errorf("binding %q principal.id: %w", binding.name, err)
		}
		scope, err := renderPrincipalTemplate(binding.principal.Scope, token, tokenVars)
		if err != nil {
			return BoundPrincipal{}, fmt.Errorf("binding %q principal.scope: %w", binding.name, err)
		}
		return BoundPrincipal{
			Principal: Principal{
				ID:      principalID,
				Subject: token.Subject,
				Issuer:  token.IssuerName,
				Scope:   scope,
			},
			Claims:  copyMapStringAny(token.Claims),
			Binding: binding.name,
			binding: binding,
		}, nil
	}
	return BoundPrincipal{}, ErrNoBinding
}

func (b BoundPrincipal) Authorize(req DecisionRequest) Decision {
	req.Principal = b.Principal
	if req.Claims == nil {
		req.Claims = b.Claims
	}
	req.Action = strings.TrimSpace(req.Action)
	req.Resource.Kind = strings.TrimSpace(req.Resource.Kind)
	decision := Decision{
		Allowed:   false,
		Principal: b.Principal,
		Action:    req.Action,
		Resource:  req.Resource,
		Binding:   b.Binding,
		Reason:    ReasonNoGrant,
	}
	if b.binding == nil {
		decision.Reason = ReasonNoBinding
		return decision
	}

	matchedGrant := false
	var conditionErrorDecision *Decision
	for _, grant := range b.binding.grants {
		if !grant.matches(decision.Action, req.Resource.Kind) {
			continue
		}
		matchedGrant = true
		ok, err := grant.condition.eval(grantActivation(req))
		if err != nil {
			if conditionErrorDecision == nil {
				errorDecision := decision
				errorDecision.Grant = grant.name
				errorDecision.Reason = ReasonConditionError
				conditionErrorDecision = &errorDecision
			}
			continue
		}
		if !ok {
			decision.Grant = grant.name
			decision.Reason = ReasonConditionFalse
			continue
		}
		decision.Allowed = true
		decision.Grant = grant.name
		decision.Reason = ReasonAllowed
		return decision
	}
	if conditionErrorDecision != nil {
		return *conditionErrorDecision
	}
	if matchedGrant && decision.Reason == ReasonNoGrant {
		decision.Reason = ReasonConditionFalse
	}
	return decision
}

func grantActivation(req DecisionRequest) map[string]any {
	return map[string]any{
		"principal": map[string]any{
			"id":      req.Principal.ID,
			"issuer":  req.Principal.Issuer,
			"subject": req.Principal.Subject,
			"scope":   req.Principal.Scope,
		},
		"claims": req.Claims,
		"action": req.Action,
		"resource": map[string]any{
			"kind": req.Resource.Kind,
			"id":   req.Resource.ID,
			"owner": map[string]any{
				"principal_id": req.Resource.Owner.PrincipalID,
				"scope":        req.Resource.Owner.Scope,
			},
		},
		"request": req.Request,
	}
}

func (g compiledGrant) matches(action, resourceKind string) bool {
	action = strings.TrimSpace(action)
	resourceKind = strings.TrimSpace(resourceKind)
	if _, ok := g.actions[action]; !ok {
		return false
	}
	if _, ok := g.resources[resourceKind]; !ok {
		return false
	}
	return true
}

var templateRefRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\}`)

func renderPrincipalTemplate(template string, token ValidatedToken, tokenVars map[string]any) (string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return "", nil
	}
	var renderErr error
	rendered := templateRefRE.ReplaceAllStringFunc(template, func(match string) string {
		if renderErr != nil {
			return ""
		}
		parts := templateRefRE.FindStringSubmatch(match)
		if len(parts) != 2 {
			renderErr = fmt.Errorf("malformed template reference %q", match)
			return ""
		}
		value, err := lookupTemplateValue(parts[1], token, tokenVars)
		if err != nil {
			renderErr = err
			return ""
		}
		return value
	})
	if renderErr != nil {
		return "", renderErr
	}
	if strings.Contains(rendered, "${") {
		return "", fmt.Errorf("malformed template %q", template)
	}
	return rendered, nil
}

func lookupTemplateValue(path string, token ValidatedToken, tokenVars map[string]any) (string, error) {
	switch {
	case strings.HasPrefix(path, "token."):
		key := strings.TrimPrefix(path, "token.")
		value, ok := tokenVars[key]
		if !ok {
			return "", fmt.Errorf("unknown token template field %q", path)
		}
		return templateScalar(path, value)
	case strings.HasPrefix(path, "claims."):
		key := strings.TrimPrefix(path, "claims.")
		value, ok := token.Claims[key]
		if !ok {
			return "", fmt.Errorf("missing claim %q", key)
		}
		return templateScalar(path, value)
	default:
		return "", fmt.Errorf("unsupported template reference %q", path)
	}
}

func templateScalar(path string, value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case fmt.Stringer:
		return v.String(), nil
	case bool:
		return fmt.Sprint(v), nil
	case int:
		return fmt.Sprint(v), nil
	case int64:
		return fmt.Sprint(v), nil
	case float64:
		return fmt.Sprint(v), nil
	case nil:
		return "", fmt.Errorf("template reference %q is null", path)
	default:
		return "", fmt.Errorf("template reference %q must resolve to a scalar, got %T", path, value)
	}
}

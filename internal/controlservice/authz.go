package controlservice

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
)

var ErrAuthorizationDenied = errors.New("authorization denied")

func ownerForContext(ctx context.Context) (authz.ResourceOwner, bool) {
	bound, ok := authz.BoundPrincipalFromContext(ctx)
	if !ok {
		return authz.ResourceOwner{}, false
	}
	return authz.ResourceOwner{
		PrincipalID: bound.Principal.ID,
		Scope:       bound.Principal.Scope,
	}, true
}

func (s *Service) authorizeCreate(ctx context.Context, action, resourceKind string, request map[string]any) (authz.ResourceOwner, error) {
	bound, ok := authz.BoundPrincipalFromContext(ctx)
	if !ok {
		return authz.ResourceOwner{}, nil
	}
	owner := authz.ResourceOwner{
		PrincipalID: bound.Principal.ID,
		Scope:       bound.Principal.Scope,
	}
	decision := bound.Authorize(authz.DecisionRequest{
		Action:   action,
		Resource: authz.Resource{Kind: resourceKind},
		Request:  request,
	})
	if !decision.Allowed {
		return owner, s.denyAuthorization(ctx, decision)
	}
	return owner, nil
}

func (s *Service) authorizeOwnedResource(ctx context.Context, action, resourceKind, resourceID string, owner authz.ResourceOwner, request map[string]any) error {
	bound, ok := authz.BoundPrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	resource := authz.Resource{
		Kind:  resourceKind,
		ID:    strings.TrimSpace(resourceID),
		Owner: owner,
	}
	if strings.TrimSpace(owner.PrincipalID) == "" {
		return s.denyAuthorization(ctx, authz.Decision{
			Allowed:   false,
			Principal: bound.Principal,
			Action:    action,
			Resource:  resource,
			Binding:   bound.Binding,
			Reason:    authz.ReasonMissingOwner,
		})
	}
	if owner.PrincipalID != bound.Principal.ID {
		return s.denyAuthorization(ctx, authz.Decision{
			Allowed:   false,
			Principal: bound.Principal,
			Action:    action,
			Resource:  resource,
			Binding:   bound.Binding,
			Reason:    authz.ReasonOwnerMismatch,
		})
	}
	decision := bound.Authorize(authz.DecisionRequest{
		Action:   action,
		Resource: resource,
		Request:  request,
	})
	if !decision.Allowed {
		return s.denyAuthorization(ctx, decision)
	}
	return nil
}

func ownedByContext(ctx context.Context, owner authz.ResourceOwner) bool {
	bound, ok := authz.BoundPrincipalFromContext(ctx)
	if !ok {
		return true
	}
	return strings.TrimSpace(owner.PrincipalID) != "" && owner.PrincipalID == bound.Principal.ID
}

func (s *Service) AuthorizeSandboxAction(ctx context.Context, action, sandboxID string) error {
	if _, ok := authz.BoundPrincipalFromContext(ctx); !ok {
		return nil
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return errors.New("missing sandbox_id")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sb, ok := s.sandboxes[sandboxID]
	if !ok {
		return fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	return s.authorizeOwnedResource(ctx, action, "sandbox", sandboxID, sb.Owner, nil)
}

func (s *Service) AuthorizeExecutionAction(ctx context.Context, action, sandboxID, executionID string) error {
	if _, ok := authz.BoundPrincipalFromContext(ctx); !ok {
		return nil
	}
	sandboxID = strings.TrimSpace(sandboxID)
	executionID = strings.TrimSpace(executionID)
	if executionID == "" {
		return errors.New("missing execution_id")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ex, err := s.lookupExecutionLocked(sandboxID, executionID)
	if err != nil {
		return err
	}
	return s.authorizeOwnedResource(ctx, action, "execution", ex.ID, ex.Owner, nil)
}

func authDenied(decision authz.Decision) error {
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = authz.ReasonNoGrant
	}
	decision.Reason = reason
	return authz.NewDecisionError(decision, ErrAuthorizationDenied)
}

func (s *Service) denyAuthorization(ctx context.Context, decision authz.Decision) error {
	s.logAuthorizationDeny(ctx, decision)
	return authDenied(decision)
}

func (s *Service) logAuthorizationDeny(ctx context.Context, decision authz.Decision) {
	if s == nil || s.Logger == nil {
		return
	}
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = authz.ReasonNoGrant
	}
	fields := []any{
		observability.LogFieldComponent, "authz",
		observability.LogFieldSubsystem, "controlservice",
		observability.LogFieldReasonCode, reason,
		"action", strings.TrimSpace(decision.Action),
		"resource_kind", strings.TrimSpace(decision.Resource.Kind),
	}
	if id := strings.TrimSpace(decision.Resource.ID); id != "" {
		fields = append(fields, "resource_id", id)
	}
	if principalID := strings.TrimSpace(decision.Principal.ID); principalID != "" {
		fields = append(fields, "principal_id", principalID)
	}
	if binding := strings.TrimSpace(decision.Binding); binding != "" {
		fields = append(fields, "binding", binding)
	}
	if grant := strings.TrimSpace(decision.Grant); grant != "" {
		fields = append(fields, "grant", grant)
	}
	observability.WithTraceContext(s.Logger, ctx).Warn("authorization denied", fields...)
}

func createSandboxAuthorizationRequest(backendName string, compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset, snapshotID string, effectiveResources policy.Resources) map[string]any {
	request := map[string]any{
		"backend":    strings.TrimSpace(backendName),
		"repository": repositoryAuthorizationRequest(repository, changeset),
		"image": map[string]any{
			"ref":    "",
			"digest": "",
		},
		"snapshot": map[string]any{
			"id": strings.TrimSpace(snapshotID),
		},
		"policy": map[string]any{
			"resources": map[string]any{
				"vcpus":        int64(0),
				"memory_bytes": int64(0),
				"disk_bytes":   int64(0),
			},
			"docker": map[string]any{
				"required": false,
			},
			"network_default": "",
			"network": map[string]any{
				"hosts": []string{},
				"ports": []int{},
			},
		},
		"cache": map[string]any{
			"reuse": "",
		},
	}
	if compiled == nil {
		return request
	}
	request["image"] = map[string]any{
		"ref":    strings.TrimSpace(compiled.ImageRef),
		"digest": strings.TrimSpace(compiled.ImageDigest),
	}
	hosts, ports := policyNetworkHostsAndPorts(compiled)
	request["policy"] = map[string]any{
		"resources": map[string]any{
			"vcpus":        effectiveResources.VCPUs,
			"memory_bytes": effectiveResources.MemoryBytes,
			"disk_bytes":   effectiveResources.DiskBytes,
		},
		"docker": map[string]any{
			"required": compiled.Docker.Required,
		},
		"network_default": strings.TrimSpace(compiled.NetworkDefault),
		"network": map[string]any{
			"hosts": hosts,
			"ports": ports,
		},
	}
	request["cache"] = map[string]any{
		"reuse": strings.TrimSpace(compiled.Dependencies.Reuse),
	}
	return request
}

func createExecutionAuthorizationRequest(repository *repositorycheckout.Checkout) map[string]any {
	return map[string]any{
		"repository": repositoryAuthorizationRequest(repository, nil),
	}
}

func repositoryAuthorizationRequest(repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) map[string]any {
	request := map[string]any{
		"remote_url": "",
		"commit":     "",
		"branch":     "",
		"changeset": map[string]any{
			"present":     false,
			"digest":      "",
			"tree_digest": "",
		},
	}
	if repository != nil {
		request["remote_url"] = strings.TrimSpace(repository.RemoteURL)
		request["commit"] = strings.TrimSpace(repository.CommitSHA)
		request["branch"] = strings.TrimSpace(repository.Branch)
	}
	if changeset != nil {
		request["changeset"] = map[string]any{
			"present":     true,
			"digest":      strings.TrimSpace(changeset.Digest),
			"tree_digest": strings.TrimSpace(changeset.TreeDigest),
		}
	}
	return request
}

func effectiveAuthorizationResources(cfg backend.FirecrackerConfig) policy.Resources {
	return policy.Resources{
		VCPUs:       cfg.VCPUs,
		MemoryBytes: cfg.MemoryMiB * mibBytes,
		DiskBytes:   cfg.MinimumRootFSBytes,
	}
}

func policyNetworkHostsAndPorts(compiled *policy.CompiledPolicy) ([]string, []int) {
	if compiled == nil {
		return nil, nil
	}
	hosts := map[string]struct{}{}
	ports := map[int]struct{}{}
	addRules := func(rules []policy.AllowRule) {
		for _, rule := range rules {
			host := strings.TrimSpace(rule.Host)
			if host != "" {
				hosts[host] = struct{}{}
			}
			for _, port := range rule.Ports {
				ports[port] = struct{}{}
			}
		}
	}
	addRules(compiled.Allow)
	if compiled.NetworkStages != nil {
		stages := []*policy.NetworkPolicy{
			compiled.NetworkStages.Workspace,
			compiled.NetworkStages.Dependencies,
			compiled.NetworkStages.Services,
			compiled.NetworkStages.Execution,
		}
		for _, stage := range stages {
			if stage != nil {
				addRules(stage.Allow)
			}
		}
	}

	hostList := make([]string, 0, len(hosts))
	for host := range hosts {
		hostList = append(hostList, host)
	}
	sort.Strings(hostList)
	portList := make([]int, 0, len(ports))
	for port := range ports {
		portList = append(portList, port)
	}
	sort.Ints(portList)
	return hostList, portList
}

func ownerFromSnapshotRecord(record snapshotstore.Record) authz.ResourceOwner {
	return authz.ResourceOwner{
		PrincipalID: strings.TrimSpace(record.OwnerPrincipalID),
		Scope:       strings.TrimSpace(record.OwnerScope),
	}
}

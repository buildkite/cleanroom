package controlservice

import (
	"strings"

	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/gatewayauth"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func gatewayScopeMetadata(owner authz.ResourceOwner, repository *repositorycheckout.Checkout, compiled *policy.CompiledPolicy) gatewayauth.ScopeMetadata {
	metadata := gatewayauth.ScopeMetadata{Owner: gatewayauth.Owner{
		PrincipalID: strings.TrimSpace(owner.PrincipalID),
		Scope:       strings.TrimSpace(owner.Scope),
	}}
	if strings.TrimSpace(owner.PrincipalID) == "" {
		return metadata
	}
	if repository != nil {
		if canonicalRemote, _, err := repositorycheckout.CanonicalizeRemoteURL(repository.RemoteURL); err == nil {
			if prefix, err := gatewayauth.GitRepoPrefixFromURL(canonicalRemote); err == nil && prefix != "" {
				metadata.Authorization.GitRepoPrefixes = append(metadata.Authorization.GitRepoPrefixes, prefix)
			}
		}
	}
	if compiled != nil && strings.TrimSpace(compiled.ImageRef) != "" {
		if prefix, err := gatewayauth.OCIRepoPrefixFromImageRef(compiled.ImageRef); err == nil && prefix != "" {
			metadata.Authorization.OCIRepoPrefixes = append(metadata.Authorization.OCIRepoPrefixes, prefix)
		}
	}
	return metadata
}

func (s *Service) gatewayScopeForSandbox(sandboxID string) gatewayauth.ScopeMetadata {
	return s.gatewayScopeForSandboxRepository(sandboxID, nil)
}

func (s *Service) gatewayScopeForSandboxRepository(sandboxID string, repository *repositorycheckout.Checkout) gatewayauth.ScopeMetadata {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return gatewayauth.ScopeMetadata{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sb, ok := s.sandboxes[sandboxID]
	if !ok {
		return gatewayauth.ScopeMetadata{}
	}
	scopeRepository := sb.Repository
	if repository != nil {
		scopeRepository = repository
	}
	return gatewayScopeMetadata(sb.Owner, scopeRepository, sb.Policy)
}

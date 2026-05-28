package gateway

import (
	"fmt"

	"github.com/buildkite/cleanroom/internal/gatewayauth"
)

func authorizeGitGatewayRequest(scope *SandboxScope, upstreamHost, repoPath string, requireOwner bool) error {
	if !requireOwner {
		return nil
	}
	if scope == nil || !scope.GatewayScope.HasOwner() {
		return fmt.Errorf("gateway route requires an authenticated sandbox owner")
	}
	repoKey, err := gatewayauth.GitRepoKeyFromRequest(upstreamHost, repoPath)
	if err != nil {
		return err
	}
	if !gatewayauth.AllowsGitRepo(scope.GatewayScope.Authorization.GitRepoPrefixes, repoKey) {
		return fmt.Errorf("git repository %q is not authorized for sandbox owner", repoKey)
	}
	return nil
}

func authorizeOCIGatewayRequest(scope *SandboxScope, prefix, rest string, requireOwner bool) error {
	if !requireOwner {
		return nil
	}
	if scope == nil || !scope.GatewayScope.HasOwner() {
		return fmt.Errorf("gateway route requires an authenticated sandbox owner")
	}
	repoKey, ok, err := gatewayauth.OCIRepoKeyFromPath(prefix, rest)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if !gatewayauth.AllowsOCIRepo(scope.GatewayScope.Authorization.OCIRepoPrefixes, repoKey) {
		return fmt.Errorf("OCI repository %q is not authorized for sandbox owner", repoKey)
	}
	return nil
}

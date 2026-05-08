package controlservice

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func cloneRepositoryCheckout(repository *repositorycheckout.Checkout) *repositorycheckout.Checkout {
	if repository == nil {
		return nil
	}
	return &repositorycheckout.Checkout{
		RemoteURL:      repository.RemoteURL,
		CommitSHA:      repository.CommitSHA,
		DestinationDir: repository.DestinationDir,
		Submodules:     repository.Submodules,
		Branch:         repository.Branch,
	}
}

func repositoryCheckoutsEqual(a, b *repositorycheckout.Checkout) bool {
	switch {
	case a == nil || b == nil:
		return a == nil && b == nil
	default:
		a = normalizeRepositoryCheckoutForComparison(a)
		b = normalizeRepositoryCheckoutForComparison(b)
		return a.RemoteURL == b.RemoteURL &&
			a.CommitSHA == b.CommitSHA &&
			a.DestinationDir == b.DestinationDir &&
			a.Submodules == b.Submodules &&
			a.Branch == b.Branch
	}
}

func normalizeRepositoryCheckoutForComparison(repository *repositorycheckout.Checkout) *repositorycheckout.Checkout {
	if repository == nil {
		return nil
	}
	normalized := cloneRepositoryCheckout(repository)
	normalized.RemoteURL = strings.TrimSpace(normalized.RemoteURL)
	if normalizedURL, _, err := repositorycheckout.CanonicalizeRemoteURL(normalized.RemoteURL); err == nil {
		normalized.RemoteURL = normalizedURL
	}
	normalized.CommitSHA = strings.ToLower(strings.TrimSpace(normalized.CommitSHA))
	normalized.DestinationDir = normalizeRepositoryDestinationDir(normalized.DestinationDir)
	normalized.Branch = strings.TrimSpace(normalized.Branch)
	return normalized
}

func normalizeRepositoryDestinationDir(destinationDir string) string {
	trimmed := strings.TrimSpace(destinationDir)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "/") {
		return path.Clean(trimmed)
	}
	return trimmed
}

func validateRepositoryCheckoutForPolicy(compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout) error {
	if repository == nil {
		return nil
	}
	if err := repository.ValidateBootstrap(); err != nil {
		return err
	}
	host, err := repository.NormalizeRemoteURL()
	if err != nil {
		return err
	}
	if compiled == nil {
		return errors.New("repository checkout requires a compiled policy")
	}
	if !compiled.AllowsForStage(policy.NetworkStageWorkspace, host, 443) {
		return fmt.Errorf("repository remote host %q is not allowed by workspace network policy", host)
	}
	return nil
}

func validateRepositoryScopedCreatePolicy(compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout) error {
	if compiled == nil || repository != nil {
		return nil
	}
	if len(compiled.Dependencies.Blocks) > 0 {
		return errors.New("sandbox.dependencies blocks require repository checkout")
	}
	if len(compiled.Services.Blocks) > 0 {
		return errors.New("sandbox.services blocks require repository checkout")
	}
	return nil
}

package controlservice

import (
	"errors"
	"fmt"

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
		return a.RemoteURL == b.RemoteURL &&
			a.CommitSHA == b.CommitSHA &&
			a.DestinationDir == b.DestinationDir &&
			a.Submodules == b.Submodules &&
			a.Branch == b.Branch
	}
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
	if !compiled.Allows(host, 443) {
		return fmt.Errorf("repository remote host %q is not allowed by sandbox policy", host)
	}
	return nil
}

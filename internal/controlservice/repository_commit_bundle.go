package controlservice

import (
	"errors"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func repositoryCommitBundleFromProto(proto *cleanroomv1.RepositoryCommitBundle) *repositorybundle.Bundle {
	return repositorybundle.FromProto(proto)
}

func cloneRepositoryCommitBundle(bundle *repositorybundle.Bundle) *repositorybundle.Bundle {
	if bundle == nil {
		return nil
	}
	return repositorybundle.FromProto(bundle.ToProto())
}

func validateRepositoryCommitBundleForCheckout(repository *repositorycheckout.Checkout, bundle *repositorybundle.Bundle) error {
	if bundle == nil {
		return nil
	}
	if repository == nil {
		return errors.New("repository commit bundle requires a repository checkout")
	}
	if err := bundle.ValidateForCheckout(repository); err != nil {
		return err
	}
	if err := bundle.ValidateContent(); err != nil {
		return err
	}
	return nil
}

func validateRepositoryCommitBundleForResolvedCheckout(repository *repositorycheckout.Checkout, bundle *repositorybundle.Bundle, policySource string) error {
	if bundle != nil && repository == nil && strings.HasPrefix(strings.TrimSpace(policySource), "repository:") {
		return nil
	}
	return validateRepositoryCommitBundleForCheckout(repository, bundle)
}

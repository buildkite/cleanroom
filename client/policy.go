package client

import internalpolicy "github.com/buildkite/cleanroom/internal/policy"

const (
	// PrimaryPolicyPath is the canonical repository policy path.
	PrimaryPolicyPath = internalpolicy.PrimaryPolicyPath
	// FallbackPolicyPath is the legacy Buildkite-local repository policy path.
	FallbackPolicyPath = internalpolicy.FallbackPolicyPath
)

// ErrPolicyNotFound is returned when no Cleanroom policy file is present.
var ErrPolicyNotFound = internalpolicy.ErrPolicyNotFound

// LoadPolicy loads and compiles a Cleanroom policy from root.
func LoadPolicy(root string) (*Policy, string, error) {
	compiled, source, err := internalpolicy.Loader{}.LoadAndCompile(root)
	if err != nil {
		return nil, source, err
	}
	return compiled.ToProto(), source, nil
}

package gateway

import (
	"context"
	"os"
	"sort"
	"strings"
)

// CredentialProvider resolves credentials for upstream hosts.
type CredentialProvider interface {
	Resolve(ctx context.Context, upstreamHost string) (token string, err error)
}

// EnvCredentialProvider reads credentials from environment variables at
// construction time. It maps upstream hosts to env var names:
//
//	github.com -> CLEANROOM_GITHUB_TOKEN
//	gitlab.com -> CLEANROOM_GITLAB_TOKEN
type EnvCredentialProvider struct {
	hostTokens map[string]string
}

// NewEnvCredentialProvider creates a provider from the current environment.
func NewEnvCredentialProvider() *EnvCredentialProvider {
	p := &EnvCredentialProvider{
		hostTokens: make(map[string]string),
	}
	hostEnvMap := map[string]string{
		"github.com": "CLEANROOM_GITHUB_TOKEN",
		"gitlab.com": "CLEANROOM_GITLAB_TOKEN",
	}
	for host, envVar := range hostEnvMap {
		if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
			p.hostTokens[host] = v
		}
	}
	return p
}

// Resolve returns the credential for the given upstream host. Returns empty
// string if no credential is configured.
func (p *EnvCredentialProvider) Resolve(_ context.Context, upstreamHost string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(upstreamHost))
	return p.hostTokens[host], nil
}

// ConfiguredHosts returns a sorted list of upstream hosts with configured tokens.
func (p *EnvCredentialProvider) ConfiguredHosts() []string {
	hosts := make([]string, 0, len(p.hostTokens))
	for host := range p.hostTokens {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

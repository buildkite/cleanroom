package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// CredentialProvider resolves an HTTP Authorization header for an upstream git
// remote URL. It returns values such as "Bearer <token>" or "Basic <base64>".
type CredentialProvider interface {
	Resolve(ctx context.Context, remoteURL string) (authorizationHeader string, err error)
}

// EnvCredentialProvider reads credentials from environment variables at
// construction time. It maps upstream hosts to bearer tokens:
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

// Resolve returns the credential for the given upstream remote URL. Returns an
// empty string if no credential is configured.
func (p *EnvCredentialProvider) Resolve(_ context.Context, remoteURL string) (string, error) {
	host, err := remoteURLHost(remoteURL)
	if err != nil {
		return "", err
	}
	token := p.hostTokens[host]
	if token == "" {
		return "", nil
	}
	return "Bearer " + token, nil
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

type gitCredentialFillFunc func(dir, input string) (string, error)

// GitCredentialFillProvider resolves path-scoped credentials from the host git
// credential helpers.
type GitCredentialFillProvider struct {
	dir  string
	fill gitCredentialFillFunc
}

func NewGitCredentialFillProvider(dir string, fill gitCredentialFillFunc) *GitCredentialFillProvider {
	if fill == nil {
		fill = gitCredentialFillFromHost
	}
	return &GitCredentialFillProvider{
		dir:  dir,
		fill: fill,
	}
}

func (p *GitCredentialFillProvider) Resolve(_ context.Context, remoteURL string) (string, error) {
	lookup, err := buildGitCredentialFillLookup(remoteURL)
	if err != nil {
		return "", err
	}
	output, err := p.fill(p.dir, lookup)
	if err != nil {
		return "", nil
	}
	username, password := parseGitCredentialFillOutput(output)
	if username == "" || password == "" {
		return "", nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + auth, nil
}

type chainCredentialProvider struct {
	providers []CredentialProvider
}

func NewChainCredentialProvider(providers ...CredentialProvider) CredentialProvider {
	filtered := make([]CredentialProvider, 0, len(providers))
	for _, provider := range providers {
		if provider != nil {
			filtered = append(filtered, provider)
		}
	}
	return chainCredentialProvider{providers: filtered}
}

func (p chainCredentialProvider) Resolve(ctx context.Context, remoteURL string) (string, error) {
	for _, provider := range p.providers {
		header, err := provider.Resolve(ctx, remoteURL)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(header) != "" {
			return header, nil
		}
	}
	return "", nil
}

func gitCredentialFillFromHost(dir, input string) (string, error) {
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func buildGitCredentialFillLookup(remoteURL string) (string, error) {
	parsed, err := normalizeCredentialURL(remoteURL)
	if err != nil {
		return "", err
	}

	var lookup strings.Builder
	lookup.WriteString("protocol=https\n")
	lookup.WriteString("host=")
	lookup.WriteString(parsed.Host)
	lookup.WriteString("\n")
	path := strings.TrimPrefix(strings.TrimSpace(parsed.Path), "/")
	if path != "" {
		lookup.WriteString("path=")
		lookup.WriteString(path)
		lookup.WriteString("\n")
	}
	lookup.WriteString("\n")
	return lookup.String(), nil
}

func parseGitCredentialFillOutput(raw string) (string, string) {
	var username string
	var password string

	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "username":
			username = strings.TrimSpace(value)
		case "password":
			password = strings.TrimSpace(value)
		}
	}
	return username, password
}

func remoteURLHost(remoteURL string) (string, error) {
	parsed, err := normalizeCredentialURL(remoteURL)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(parsed.Hostname())), nil
}

func normalizeCredentialURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("empty remote URL")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse remote URL %q: %w", raw, err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("remote URL %q must use https", raw)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return nil, fmt.Errorf("remote URL %q missing host", raw)
	}
	parsed.User = nil
	return parsed, nil
}

func shouldResolveCredentialsForRawURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return shouldResolveCredentialsForURL(parsed)
}

func shouldResolveCredentialsForURL(parsed *url.URL) bool {
	return parsed != nil && strings.EqualFold(parsed.Scheme, "https")
}

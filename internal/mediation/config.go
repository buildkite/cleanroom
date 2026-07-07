// Package mediation implements the lineage-scoped cleanroom gateway: a host
// service bound into guests over a Unix socket that brokers access to
// credentialed upstreams so secrets never enter the sandbox or its captured
// artifacts.
//
// Authorization is the intersection of three layers: the repository requests
// service names in policy (recorded in provenance, hash-covered), the host
// grants services to lineages via this config (matched on verified
// provenance facts), and the operator's act of binding the served socket
// into a VM connects the two. Attribution is per guest and presented by the
// guest; it never influences authorization.
package mediation

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath returns the XDG location of the gateway grants config.
func DefaultConfigPath() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "cleanroom", "gateway.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "cleanroom", "gateway.yaml")
}

// Config is the operator-side gateway configuration: what each service name
// means on this host, and which lineages qualify for which services.
type Config struct {
	Services map[string]ServiceDefinition `yaml:"services"`
	Grants   []Grant                      `yaml:"grants"`
}

// ServiceDefinition maps a service name to a concrete upstream and the
// host-side credential attached to forwarded requests.
type ServiceDefinition struct {
	// Upstream is the base URL requests are forwarded to.
	Upstream string `yaml:"upstream"`
	// AllowedPathPrefixes optionally narrows which service-relative paths can
	// be proxied. Nil means no path restriction; an explicit empty list denies
	// every path.
	AllowedPathPrefixes []string `yaml:"allowed_path_prefixes"`
	// CredentialEnv names the host environment variable holding the secret.
	// Empty means the service forwards without credential injection.
	CredentialEnv string `yaml:"credential_env"`
	// Header is the request header carrying the credential (default
	// "Authorization"; the value is sent verbatim unless HeaderPrefix set).
	Header string `yaml:"header"`
	// HeaderPrefix is prepended to the credential, e.g. "Bearer ".
	HeaderPrefix string `yaml:"header_prefix"`
}

// Grant allows a set of services to lineages whose verified provenance
// matches. All specified match fields must match; dirty workspaces never
// match any grant.
type Grant struct {
	Match    GrantMatch `yaml:"match"`
	Services []string   `yaml:"services"`
}

// GrantMatch selects lineages by verified provenance facts. Remote supports
// a trailing-glob pattern ("https://github.com/example-org/*"); PolicyHash
// is exact.
type GrantMatch struct {
	Remote     string `yaml:"remote"`
	PolicyHash string `yaml:"policy_hash"`
}

// LoadConfig reads and validates the grants config.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read gateway config %s: %w", path, err)
	}
	var config Config
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("parse gateway config %s: %w", path, err)
	}
	if err := config.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid gateway config %s: %w", path, err)
	}
	return config, nil
}

func (c Config) validate() error {
	for name, service := range c.Services {
		if strings.TrimSpace(name) == "" {
			return errors.New("service with empty name")
		}
		upstream := strings.TrimSpace(service.Upstream)
		if upstream == "" {
			return fmt.Errorf("service %s is missing upstream", name)
		}
		parsed, err := url.Parse(upstream)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("service %s has invalid upstream %q: must be an http(s) URL", name, upstream)
		}
		for _, prefix := range service.AllowedPathPrefixes {
			if !isCanonicalPathPrefix(prefix) {
				return fmt.Errorf("service %s has invalid allowed_path_prefixes entry %q: must be an absolute canonical path prefix", name, prefix)
			}
		}
	}
	for i, grant := range c.Grants {
		if grant.Match.Remote == "" && grant.Match.PolicyHash == "" {
			return fmt.Errorf("grant %d has an empty match; require at least a remote or policy hash", i)
		}
		if len(grant.Services) == 0 {
			return fmt.Errorf("grant %d grants no services", i)
		}
		for _, name := range grant.Services {
			if _, ok := c.Services[name]; !ok {
				return fmt.Errorf("grant %d references undefined service %q", i, name)
			}
		}
	}
	return nil
}

func isCanonicalPathPrefix(prefix string) bool {
	if prefix == "" || !strings.HasPrefix(prefix, "/") {
		return false
	}
	cleaned := path.Clean(prefix)
	if strings.HasSuffix(prefix, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned == prefix
}

// LineageFacts are the verified provenance facts grants match on.
type LineageFacts struct {
	Remote     string
	PolicyHash string
	Dirty      bool
}

// Granted returns the service names this config grants to a lineage.
// Dirty workspaces receive no grants.
func (c Config) Granted(facts LineageFacts) []string {
	if facts.Dirty {
		return nil
	}
	granted := map[string]bool{}
	for _, grant := range c.Grants {
		if !grant.Match.matches(facts) {
			continue
		}
		for _, name := range grant.Services {
			granted[name] = true
		}
	}
	names := make([]string, 0, len(granted))
	for name := range granted {
		names = append(names, name)
	}
	return names
}

func (m GrantMatch) matches(facts LineageFacts) bool {
	if m.PolicyHash != "" && m.PolicyHash != facts.PolicyHash {
		return false
	}
	if m.Remote != "" && !remoteMatches(m.Remote, facts.Remote) {
		return false
	}
	return true
}

func remoteMatches(pattern, remote string) bool {
	if remote == "" {
		return false
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(remote, prefix)
	}
	return pattern == remote
}

// ResolveScope intersects the lineage's requested services with the host's
// grants and returns the service definitions the gateway will serve. It
// fails closed when any requested service is undefined or ungranted, so a
// gateway never silently serves a narrower scope than the artifact expects.
func ResolveScope(config Config, requested []string, facts LineageFacts) (map[string]ServiceDefinition, error) {
	if len(requested) == 0 {
		return nil, errors.New("the lineage requests no mediation services; nothing to serve")
	}
	granted := map[string]bool{}
	for _, name := range config.Granted(facts) {
		granted[name] = true
	}
	scope := make(map[string]ServiceDefinition, len(requested))
	for _, name := range requested {
		definition, defined := config.Services[name]
		if !defined {
			return nil, fmt.Errorf("requested service %q is not defined in the gateway config", name)
		}
		if !granted[name] {
			return nil, fmt.Errorf("requested service %q is not granted to this lineage (remote %q, policy %.12s, dirty=%t)", name, facts.Remote, facts.PolicyHash, facts.Dirty)
		}
		scope[name] = definition
	}
	return scope, nil
}

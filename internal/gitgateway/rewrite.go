package gitgateway

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type RewriteRule struct {
	BaseURL   string
	InsteadOf string
}

// BuildRewriteRulesForScope returns stable, per-host insteadOf rules which
// rewrite https remotes to a scope-specific local gateway endpoint.
func BuildRewriteRulesForScope(relayBaseURL, scope string, allowedHosts []string) ([]RewriteRule, error) {
	trimmedBase := strings.TrimSpace(relayBaseURL)
	if trimmedBase == "" {
		return nil, fmt.Errorf("relay base URL is required")
	}
	parsedBase, err := url.Parse(trimmedBase)
	if err != nil {
		return nil, fmt.Errorf("invalid relay base URL %q: %w", relayBaseURL, err)
	}
	if parsedBase.Scheme != "http" && parsedBase.Scheme != "https" {
		return nil, fmt.Errorf("relay base URL %q must use http or https", relayBaseURL)
	}
	if parsedBase.Host == "" {
		return nil, fmt.Errorf("relay base URL %q must include a host", relayBaseURL)
	}

	normalizedHosts := make([]string, 0, len(allowedHosts))
	seen := map[string]struct{}{}
	for _, host := range allowedHosts {
		normalized := strings.ToLower(strings.TrimSpace(host))
		if normalized == "" {
			return nil, fmt.Errorf("allowed hosts cannot contain empty entries")
		}
		if strings.Contains(normalized, "/") {
			return nil, fmt.Errorf("allowed host %q must be a hostname", normalized)
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		normalizedHosts = append(normalizedHosts, normalized)
	}
	if len(normalizedHosts) == 0 {
		return nil, fmt.Errorf("at least one allowed host is required")
	}
	sort.Strings(normalizedHosts)

	basePrefix := strings.TrimRight(parsedBase.String(), "/")
	scopePrefix := ""
	trimmedScope := strings.TrimSpace(scope)
	if trimmedScope != "" {
		scopePrefix = trimmedScope + "/"
	}
	rules := make([]RewriteRule, 0, len(normalizedHosts))
	for _, host := range normalizedHosts {
		rules = append(rules, RewriteRule{
			BaseURL:   fmt.Sprintf("%s/git/%s%s/", basePrefix, scopePrefix, host),
			InsteadOf: fmt.Sprintf("https://%s/", host),
		})
	}

	return rules, nil
}

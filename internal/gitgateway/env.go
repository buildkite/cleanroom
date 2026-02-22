package gitgateway

import "fmt"

// BuildScopedGitConfigEnv builds GIT_CONFIG_COUNT style environment variables
// for temporary per-process URL rewrite rules.
func BuildScopedGitConfigEnv(rules []RewriteRule) []string {
	if len(rules) == 0 {
		return nil
	}

	out := make([]string, 0, 1+(len(rules)*2))
	out = append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(rules)))
	for i, rule := range rules {
		out = append(out,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=url.%s.insteadof", i, rule.BaseURL),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, rule.InsteadOf),
		)
	}

	return out
}

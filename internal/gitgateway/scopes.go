package gitgateway

import (
	"fmt"
	"strings"
	"sync"

	"github.com/buildkite/cleanroom/internal/policy"
)

type ScopeRegistry struct {
	mu       sync.RWMutex
	policies map[string]*policy.GitPolicy
}

func NewScopeRegistry() *ScopeRegistry {
	return &ScopeRegistry{policies: map[string]*policy.GitPolicy{}}
}

func (r *ScopeRegistry) Set(scope string, gitPolicy *policy.GitPolicy) error {
	if r == nil {
		return fmt.Errorf("scope registry is nil")
	}
	normalizedScope := strings.TrimSpace(scope)
	if normalizedScope == "" {
		return fmt.Errorf("scope is required")
	}
	if gitPolicy == nil || !gitPolicy.Enabled {
		return fmt.Errorf("enabled git policy is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[normalizedScope] = clonePolicy(gitPolicy)
	return nil
}

func (r *ScopeRegistry) Resolve(scope string) (*policy.GitPolicy, bool) {
	if r == nil {
		return nil, false
	}
	normalizedScope := strings.TrimSpace(scope)
	if normalizedScope == "" {
		return nil, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.policies[normalizedScope]
	if !ok {
		return nil, false
	}
	return clonePolicy(p), true
}

func (r *ScopeRegistry) Delete(scope string) int {
	if r == nil {
		return 0
	}
	normalizedScope := strings.TrimSpace(scope)
	if normalizedScope == "" {
		return r.Count()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.policies, normalizedScope)
	return len(r.policies)
}

func (r *ScopeRegistry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.policies)
}

func clonePolicy(gitPolicy *policy.GitPolicy) *policy.GitPolicy {
	if gitPolicy == nil {
		return nil
	}
	copyPolicy := *gitPolicy
	copyPolicy.AllowedHosts = append([]string(nil), gitPolicy.AllowedHosts...)
	copyPolicy.AllowedRepos = append([]string(nil), gitPolicy.AllowedRepos...)
	return &copyPolicy
}

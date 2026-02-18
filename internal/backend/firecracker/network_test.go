package firecracker

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestResolveForwardRulesWithLookupDeduplicates(t *testing.T) {
	t.Parallel()

	allow := []policy.AllowRule{
		{Host: "registry.npmjs.org", Ports: []int{443, 443}},
	}
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		if host != "registry.npmjs.org" {
			t.Fatalf("unexpected host lookup %q", host)
		}
		return []net.IP{net.ParseIP("203.0.113.7"), net.ParseIP("203.0.113.7")}, nil
	}

	rules, err := resolveForwardRulesWithLookup(context.Background(), allow, lookup)
	if err != nil {
		t.Fatalf("resolve rules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 unique rules (tcp+udp), got %d", len(rules))
	}
	seen := map[string]struct{}{}
	for _, r := range rules {
		seen[r.Protocol+"|"+r.DestIP+"|"+strconv.Itoa(r.DestPort)] = struct{}{}
	}
	for _, k := range []string{"tcp|203.0.113.7|443", "udp|203.0.113.7|443"} {
		if _, ok := seen[k]; !ok {
			t.Fatalf("missing rule %s", k)
		}
	}
}

func TestResolveForwardRulesWithLookupReturnsLookupError(t *testing.T) {
	t.Parallel()

	lookup := func(_ context.Context, _ string) ([]net.IP, error) {
		return nil, errors.New("dns down")
	}
	_, err := resolveForwardRulesWithLookup(context.Background(), []policy.AllowRule{{Host: "example.com", Ports: []int{443}}}, lookup)
	if err == nil || !strings.Contains(err.Error(), "dns down") {
		t.Fatalf("expected dns error, got %v", err)
	}
}

func TestSetupHostNetworkWithDepsAddsDenyDefaultAndCleanupIndependentContext(t *testing.T) {
	t.Parallel()

	type call struct {
		ctxCanceled bool
		args        []string
	}
	var calls []call
	run := func(ctx context.Context, args ...string) error {
		copied := append([]string(nil), args...)
		calls = append(calls, call{ctxCanceled: ctx.Err() != nil, args: copied})
		return nil
	}
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		if host != "proxy.golang.org" {
			t.Fatalf("unexpected host %q", host)
		}
		return []net.IP{net.ParseIP("142.251.41.17")}, nil
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	cfg, cleanup, err := setupHostNetworkWithDeps(reqCtx, "run-12345", []policy.AllowRule{{Host: "proxy.golang.org", Ports: []int{443}}}, lookup, run)
	if err != nil {
		t.Fatalf("setupHostNetworkWithDeps: %v", err)
	}
	cancel()
	cleanup()

	tap := cfg.TapName
	if tap == "" {
		t.Fatal("expected non-empty tap name")
	}
	haystack := make([]string, 0, len(calls))
	for _, c := range calls {
		haystack = append(haystack, strings.Join(c.args, " "))
	}
	joined := strings.Join(haystack, "\n")
	if !strings.Contains(joined, "iptables -A FORWARD -i "+tap+" -j DROP") {
		t.Fatalf("expected default deny FORWARD rule for tap %s\ncalls:\n%s", tap, joined)
	}
	if strings.Contains(joined, "iptables -A FORWARD -i "+tap+" -j ACCEPT") {
		t.Fatalf("unexpected blanket ACCEPT FORWARD rule for tap %s\ncalls:\n%s", tap, joined)
	}
	if !strings.Contains(joined, "iptables -A FORWARD -i "+tap+" -p tcp -d 142.251.41.17 --dport 443 -j ACCEPT") {
		t.Fatalf("expected tcp allow rule for policy host\ncalls:\n%s", joined)
	}
	if !strings.Contains(joined, "iptables -A FORWARD -i "+tap+" -p udp -d 142.251.41.17 --dport 443 -j ACCEPT") {
		t.Fatalf("expected udp allow rule for policy host\ncalls:\n%s", joined)
	}

	cleanupCalls := 0
	for _, c := range calls {
		line := strings.Join(c.args, " ")
		if strings.Contains(line, " -D ") || strings.HasPrefix(line, "ip link del ") {
			cleanupCalls++
			if c.ctxCanceled {
				t.Fatalf("cleanup command ran with canceled context: %s", line)
			}
		}
	}
	if cleanupCalls == 0 {
		t.Fatal("expected cleanup commands")
	}
}

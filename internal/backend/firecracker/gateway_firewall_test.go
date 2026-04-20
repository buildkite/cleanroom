package firecracker

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	charmlog "github.com/charmbracelet/log"
)

func TestSetupGatewayFirewall(t *testing.T) {
	t.Parallel()

	var calls []string
	run := func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}

	cleanup, err := setupGatewayFirewallWithRunner(context.Background(), 8170, testPrivilegedCommandRunner{run: run})
	if err != nil {
		t.Fatalf("setupGatewayFirewall: %v", err)
	}

	want := []string{
		"iptables -A INPUT -i lo -p tcp --dport 8170 -j ACCEPT",
		"iptables -A INPUT ! -i cr+ -p tcp --dport 8170 -j DROP",
	}
	if len(calls) != len(want) {
		t.Fatalf("expected %d setup calls, got %d:\n%s", len(want), len(calls), strings.Join(calls, "\n"))
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("setup call %d:\n  got:  %s\n  want: %s", i, calls[i], w)
		}
	}

	calls = nil
	cleanup()
	wantCleanup := []string{
		"iptables -D INPUT ! -i cr+ -p tcp --dport 8170 -j DROP",
		"iptables -D INPUT -i lo -p tcp --dport 8170 -j ACCEPT",
	}
	if len(calls) != len(wantCleanup) {
		t.Fatalf("expected %d cleanup calls, got %d:\n%s", len(wantCleanup), len(calls), strings.Join(calls, "\n"))
	}
	for i, w := range wantCleanup {
		if calls[i] != w {
			t.Errorf("cleanup call %d:\n  got:  %s\n  want: %s", i, calls[i], w)
		}
	}
}

func TestSetupGatewayFirewallPassesLoggerToHostRuntime(t *testing.T) {
	prevHostRuntimeFn := newHostRuntimeFn
	t.Cleanup(func() { newHostRuntimeFn = prevHostRuntimeFn })

	logger := charmlog.New(io.Discard)
	runtime := &testLoggerAwareHostRuntime{}
	runtime.testHostRuntime.setupGatewayFirewallFn = func(_ context.Context, req gatewayFirewallRequest) (gatewayFirewallLease, error) {
		if got, want := req.Port, 8170; got != want {
			t.Fatalf("unexpected gateway firewall port: got %d want %d", got, want)
		}
		if runtime.logger != logger {
			t.Fatal("expected SetupGatewayFirewall to pass the logger into the host runtime")
		}
		return gatewayFirewallLease{}, nil
	}
	newHostRuntimeFn = func(backend.FirecrackerConfig) hostRuntime {
		return runtime
	}

	cleanup, err := SetupGatewayFirewall(context.Background(), 8170, backend.FirecrackerConfig{}, logger)
	if err != nil {
		t.Fatalf("SetupGatewayFirewall returned error: %v", err)
	}
	cleanup()
}

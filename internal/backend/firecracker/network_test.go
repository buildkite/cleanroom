package firecracker

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/dnsproxy"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/miekg/dns"
)

func TestTrustedDNSSetSyncerSyncsRuntimeObservationsToIPSets(t *testing.T) {
	t.Parallel()

	runtime := dnsproxy.NewRuntime(dnsproxy.RuntimeConfig{
		MaxObservationsPerScope:  8,
		MaxConnectionsPerSandbox: 8,
	})
	if err := runtime.RegisterSandbox("sandbox-1", &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "service.example", Ports: []int{443}},
			{Host: "cdn.example", Ports: []int{8443}},
		},
	}); err != nil {
		t.Fatalf("register sandbox: %v", err)
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")
	if err := runtime.ObserveResponse("sandbox-1", sourceIP, testDNSResponse("service.example.",
		&dns.CNAME{
			Hdr:    dns.RR_Header{Name: "service.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 5},
			Target: "cdn.example.",
		},
		&dns.A{
			Hdr: dns.RR_Header{Name: "cdn.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.ParseIP("203.0.113.60"),
		},
	), now); err != nil {
		t.Fatalf("observe response: %v", err)
	}

	var calls []string
	runBatch := func(_ context.Context, commands [][]string) error {
		for _, command := range commands {
			calls = append(calls, strings.Join(command, " "))
		}
		return nil
	}

	syncer := trustedDNSSetSyncer{
		sandboxID:  "sandbox-1",
		sourceIP:   sourceIP,
		runtime:    runtime,
		tcpSetName: trustedDNSTCPSetName("crrun12345"),
		udpSetName: trustedDNSUDPSetName("crrun12345"),
		runBatch:   runBatch,
		now:        func() time.Time { return now },
	}

	if err := syncer.Sync(context.Background()); err != nil {
		t.Fatalf("sync trusted dns sets: %v", err)
	}

	joined := strings.Join(calls, "\n")
	for _, line := range []string{
		"ipset flush " + trustedDNSTCPSetName("crrun12345"),
		"ipset flush " + trustedDNSUDPSetName("crrun12345"),
		"ipset add " + trustedDNSTCPSetName("crrun12345") + " 203.0.113.60,tcp:443 timeout 5",
		"ipset add " + trustedDNSUDPSetName("crrun12345") + " 203.0.113.60,udp:443 timeout 5",
		"ipset add " + trustedDNSTCPSetName("crrun12345") + " 203.0.113.60,tcp:8443 timeout 5",
		"ipset add " + trustedDNSUDPSetName("crrun12345") + " 203.0.113.60,udp:8443 timeout 5",
	} {
		if !strings.Contains(joined, line) {
			t.Fatalf("missing sync command %q\ncommands:\n%s", line, joined)
		}
	}
}

func TestSetupHostNetworkWithTrustedDNSFactoryConfiguresDynamicRulesWithoutStaticResolution(t *testing.T) {
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
	runBatch := func(ctx context.Context, commands [][]string) error {
		for _, args := range commands {
			copied := append([]string(nil), args...)
			calls = append(calls, call{ctxCanceled: ctx.Err() != nil, args: copied})
		}
		return nil
	}
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		t.Fatalf("trusted dns setup should not resolve policy host %q during network setup", host)
		return nil, nil
	}

	var dnsCfg trustedDNSConfig
	factory := func(_ context.Context, cfg trustedDNSConfig) (func(), error) {
		dnsCfg = cfg
		return func() {}, nil
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	cfg, cleanup, err := setupHostNetworkWithTrustedDNSFactory(reqCtx, "run-12345", false, []policy.AllowRule{{Host: "proxy.golang.org", Ports: []int{443}}}, 8170, lookup, net.InterfaceByName, run, runBatch, factory)
	if err != nil {
		t.Fatalf("setupHostNetworkWithTrustedDNSFactory: %v", err)
	}
	cancel()
	cleanup()

	tap := cfg.TapName
	if got, want := cfg.GuestDNS, cfg.HostIP; got != want {
		t.Fatalf("unexpected guest dns target: got %q want %q", got, want)
	}
	if got, want := dnsCfg.sandboxID, "run-12345"; got != want {
		t.Fatalf("unexpected trusted dns sandbox id: got %q want %q", got, want)
	}
	if got, want := dnsCfg.hostIP, netip.MustParseAddr(cfg.HostIP); got != want {
		t.Fatalf("unexpected trusted dns host ip: got %s want %s", got, want)
	}
	if got, want := dnsCfg.guestIP, netip.MustParseAddr(cfg.GuestIP); got != want {
		t.Fatalf("unexpected trusted dns guest ip: got %s want %s", got, want)
	}

	joinedLines := make([]string, 0, len(calls))
	for _, call := range calls {
		joinedLines = append(joinedLines, strings.Join(call.args, " "))
	}
	joined := strings.Join(joinedLines, "\n")

	for _, expected := range []string{
		"ipset create " + trustedDNSTCPSetName(tap) + " hash:ip,port family inet timeout 1",
		"ipset create " + trustedDNSUDPSetName(tap) + " hash:ip,port family inet timeout 1",
		"iptables -t nat -A PREROUTING -i " + tap + " -p udp --dport 53 -j REDIRECT --to-ports " + strconv.Itoa(trustedDNSListenPort),
		"iptables -t nat -A PREROUTING -i " + tap + " -p tcp --dport 53 -j REDIRECT --to-ports " + strconv.Itoa(trustedDNSListenPort),
		"iptables -A FORWARD -i " + tap + " -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
		"iptables -A FORWARD -i " + tap + " -p tcp -m set --match-set " + trustedDNSTCPSetName(tap) + " dst,dst -j ACCEPT",
		"iptables -A FORWARD -i " + tap + " -p udp -m set --match-set " + trustedDNSUDPSetName(tap) + " dst,dst -j ACCEPT",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing command %q\ncalls:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "proxy.golang.org") || strings.Contains(joined, "142.251.") {
		t.Fatalf("did not expect static host resolution in setup commands\ncalls:\n%s", joined)
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
	runBatch := func(ctx context.Context, commands [][]string) error {
		for _, args := range commands {
			copied := append([]string(nil), args...)
			calls = append(calls, call{ctxCanceled: ctx.Err() != nil, args: copied})
		}
		return nil
	}
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		t.Fatalf("trusted dns setup should not resolve policy host %q during network setup", host)
		return nil, nil
	}
	factory := func(_ context.Context, _ trustedDNSConfig) (func(), error) {
		return func() {}, nil
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	cfg, cleanup, err := setupHostNetworkWithTrustedDNSFactory(reqCtx, "run-12345", false, []policy.AllowRule{{Host: "proxy.golang.org", Ports: []int{443}}}, 8170, lookup, net.InterfaceByName, run, runBatch, factory)
	if err != nil {
		t.Fatalf("setupHostNetworkWithTrustedDNSFactory: %v", err)
	}
	cancel()
	cleanup()

	tap := cfg.TapName
	if tap == "" {
		t.Fatal("expected non-empty tap name")
	}
	if cfg.PolicyResolveMS != 0 {
		t.Fatalf("expected trusted dns networking to avoid setup-time resolution, got %d", cfg.PolicyResolveMS)
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
	if !strings.Contains(joined, "iptables -A FORWARD -i "+tap+" -p tcp -m set --match-set "+trustedDNSTCPSetName(tap)+" dst,dst -j ACCEPT") {
		t.Fatalf("expected dynamic tcp set rule for policy host\ncalls:\n%s", joined)
	}
	if !strings.Contains(joined, "iptables -A FORWARD -i "+tap+" -p udp -m set --match-set "+trustedDNSUDPSetName(tap)+" dst,dst -j ACCEPT") {
		t.Fatalf("expected dynamic udp set rule for policy host\ncalls:\n%s", joined)
	}
	if strings.Contains(joined, "142.251.41.17") {
		t.Fatalf("did not expect static resolved ip rules in setup\ncalls:\n%s", joined)
	}

	// Verify anti-spoof INPUT rules.
	if !strings.Contains(joined, "iptables -A INPUT -i "+tap+" ! -s "+cfg.GuestIP+" -j DROP") {
		t.Fatalf("expected anti-spoof INPUT rule for tap %s\ncalls:\n%s", tap, joined)
	}
	if !strings.Contains(joined, "iptables -A INPUT -i "+tap+" -s "+cfg.GuestIP+" -p tcp --dport 8170 -j ACCEPT") {
		t.Fatalf("expected gateway INPUT ACCEPT rule for tap %s\ncalls:\n%s", tap, joined)
	}
	if !strings.Contains(joined, "iptables -A INPUT -i "+tap+" -j DROP") {
		t.Fatalf("expected INPUT catch-all DROP rule for tap %s\ncalls:\n%s", tap, joined)
	}

	// Verify INPUT rules appear before FORWARD rules.
	inputAntiSpoofIdx := strings.Index(joined, "iptables -A INPUT -i "+tap+" ! -s ")
	forwardIdx := strings.Index(joined, "iptables -A FORWARD -i "+tap)
	if inputAntiSpoofIdx < 0 || forwardIdx < 0 || inputAntiSpoofIdx > forwardIdx {
		t.Fatalf("INPUT rules must appear before FORWARD rules\ncalls:\n%s", joined)
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

func TestSetupHostNetworkWithTapLookupDeletesStaleTapBeforeCreate(t *testing.T) {
	t.Parallel()

	runID := "run-12345"
	tapName := tapNameFromExecutionID(runID)
	staleTapExists := true

	var calls [][]string
	run := func(_ context.Context, args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if isTapDeleteCommand(args, tapName) {
			staleTapExists = false
		}
		return nil
	}
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		t.Fatalf("trusted dns setup should not resolve policy host %q during network setup", host)
		return nil, nil
	}
	interfaceByName := func(name string) (*net.Interface, error) {
		if name != tapName {
			t.Fatalf("unexpected interface lookup %q", name)
		}
		if staleTapExists {
			return &net.Interface{Name: name}, nil
		}
		return nil, errors.New("no such network interface")
	}

	factory := func(_ context.Context, _ trustedDNSConfig) (func(), error) {
		return func() {}, nil
	}
	_, cleanup, err := setupHostNetworkWithTrustedDNSFactory(context.Background(), runID, false, []policy.AllowRule{{Host: "proxy.golang.org", Ports: []int{443}}}, 0, lookup, interfaceByName, run, nil, factory)
	if err != nil {
		t.Fatalf("setupHostNetworkWithTrustedDNSFactory: %v", err)
	}
	defer cleanup()

	if len(calls) < 2 {
		t.Fatalf("expected at least two commands, got %d", len(calls))
	}
	if got, want := strings.Join(calls[0], " "), "ip link del "+tapName; got != want {
		t.Fatalf("unexpected first command: got %q want %q", got, want)
	}
	if got, want := strings.Join(calls[1], " "), "ip tuntap add dev "+tapName+" mode tap user "+strconv.Itoa(os.Getuid()); got != want {
		t.Fatalf("unexpected second command: got %q want %q", got, want)
	}
}

func TestSetupHostNetworkWithDepsAddsAllowAllForwardRule(t *testing.T) {
	t.Parallel()

	var calls []string
	run := func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	runBatch := func(_ context.Context, commands [][]string) error {
		for _, args := range commands {
			calls = append(calls, strings.Join(args, " "))
		}
		return nil
	}
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		t.Fatalf("allow-all networking should not resolve policy host %q", host)
		return nil, nil
	}

	factory := func(_ context.Context, _ trustedDNSConfig) (func(), error) {
		return func() {}, nil
	}
	cfg, cleanup, err := setupHostNetworkWithTrustedDNSFactory(context.Background(), "run-allow-all", true, []policy.AllowRule{{Host: "stale.example.invalid", Ports: []int{443}}}, 0, lookup, net.InterfaceByName, run, runBatch, factory)
	if err != nil {
		t.Fatalf("setupHostNetworkWithTrustedDNSFactory: %v", err)
	}
	defer cleanup()

	joined := strings.Join(calls, "\n")
	tap := cfg.TapName
	if !strings.Contains(joined, "iptables -A FORWARD -i "+tap+" -j ACCEPT") {
		t.Fatalf("expected blanket ACCEPT FORWARD rule for tap %s\ncalls:\n%s", tap, joined)
	}
	if strings.Contains(joined, "iptables -A FORWARD -i "+tap+" -j DROP") {
		t.Fatalf("unexpected default DROP FORWARD rule for tap %s\ncalls:\n%s", tap, joined)
	}
	if cfg.PolicyResolveMS != 0 {
		t.Fatalf("expected allow-all networking to skip policy resolution timing, got %d", cfg.PolicyResolveMS)
	}
}

func testDNSResponse(query string, answers ...dns.RR) *dns.Msg {
	msg := new(dns.Msg)
	msg.SetQuestion(query, dns.TypeA)
	msg.Response = true
	msg.Answer = append(msg.Answer, answers...)
	return msg
}

func TestDeleteTapDeviceWithRetryRetriesBusyTapDeletion(t *testing.T) {
	t.Parallel()

	tapName := "tap0"
	tapExists := true
	attempts := 0
	run := func(_ context.Context, args ...string) error {
		if got, want := strings.Join(args, " "), "ip link del "+tapName; got != want {
			t.Fatalf("unexpected delete command: got %q want %q", got, want)
		}
		attempts++
		if attempts == 1 {
			return errors.New("device busy")
		}
		tapExists = false
		return nil
	}
	interfaceByName := func(name string) (*net.Interface, error) {
		if name != tapName {
			t.Fatalf("unexpected interface lookup %q", name)
		}
		if tapExists {
			return &net.Interface{Name: name}, nil
		}
		return nil, errors.New("no such network interface")
	}

	if err := deleteTapDeviceWithRetry(context.Background(), tapName, time.Millisecond, interfaceByName, run); err != nil {
		t.Fatalf("deleteTapDeviceWithRetry: %v", err)
	}
	if got, want := attempts, 2; got != want {
		t.Fatalf("unexpected delete attempts: got %d want %d", got, want)
	}
}

func TestDeleteTapDeviceWithRetryReturnsLookupError(t *testing.T) {
	t.Parallel()

	tapName := "tap0"
	attempts := 0
	run := func(_ context.Context, args ...string) error {
		if got, want := strings.Join(args, " "), "ip link del "+tapName; got != want {
			t.Fatalf("unexpected delete command: got %q want %q", got, want)
		}
		attempts++
		return errors.New("device busy")
	}
	interfaceByName := func(name string) (*net.Interface, error) {
		if name != tapName {
			t.Fatalf("unexpected interface lookup %q", name)
		}
		return nil, errors.New("permission denied")
	}

	err := deleteTapDeviceWithRetry(context.Background(), tapName, time.Millisecond, interfaceByName, run)
	if err == nil {
		t.Fatal("expected lookup error")
	}
	if !strings.Contains(err.Error(), "lookup tap device") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("unexpected lookup error: %v", err)
	}
	if got, want := attempts, 1; got != want {
		t.Fatalf("unexpected delete attempts: got %d want %d", got, want)
	}
}

func TestInstallForwardReturnPathRuleFallsBackToStateModule(t *testing.T) {
	t.Parallel()

	var calls [][]string
	run := func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "-m conntrack") {
			return errors.New("conntrack not supported")
		}
		return nil
	}

	cleanup, err := installForwardReturnPathRule(run, "tap0")
	if err != nil {
		t.Fatalf("installForwardReturnPathRule returned error: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected two attempts (conntrack then state), got %d", len(calls))
	}
	if got := strings.Join(calls[0], " "); !strings.Contains(got, "-m conntrack") {
		t.Fatalf("expected first attempt to use conntrack, got %q", got)
	}
	if got := strings.Join(calls[1], " "); !strings.Contains(got, "-m state") {
		t.Fatalf("expected fallback to state module, got %q", got)
	}
	if got := strings.Join(cleanup, " "); !strings.Contains(got, "-m state") {
		t.Fatalf("expected cleanup rule for state module, got %q", got)
	}
}

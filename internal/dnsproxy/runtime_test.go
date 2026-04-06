package dnsproxy

import (
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/miekg/dns"
)

func TestRuntimeAllowsConnectionFromObservedAllowedAddress(t *testing.T) {
	t.Parallel()

	runtime := NewRuntime(RuntimeConfig{
		MaxObservationsPerScope:  8,
		MaxConnectionsPerSandbox: 8,
	})
	if err := runtime.RegisterSandbox("sandbox-1", testCompiledPolicy(
		policy.AllowRule{Host: "api.example.com", Ports: []int{443}},
	)); err != nil {
		t.Fatalf("register sandbox: %v", err)
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")
	destIP := netip.MustParseAddr("203.0.113.10")

	if err := runtime.ObserveResponse("sandbox-1", sourceIP, testResponse("api.example.com.",
		&dns.A{
			Hdr: dns.RR_Header{Name: "api.example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.ParseIP("203.0.113.10"),
		},
	), now); err != nil {
		t.Fatalf("observe response: %v", err)
	}

	observations := runtime.Observations("sandbox-1", now)
	if len(observations) != 1 {
		t.Fatalf("unexpected observation count: got %d want 1", len(observations))
	}
	observation := observations[0]
	if got, want := observation.SandboxID, "sandbox-1"; got != want {
		t.Fatalf("unexpected observation sandbox id: got %q want %q", got, want)
	}
	if got, want := observation.SourceIP, sourceIP; got != want {
		t.Fatalf("unexpected observation source ip: got %s want %s", got, want)
	}
	if got, want := observation.QueryName, "api.example.com"; got != want {
		t.Fatalf("unexpected query name: got %q want %q", got, want)
	}
	if got, want := observation.Name, "api.example.com"; got != want {
		t.Fatalf("unexpected record name: got %q want %q", got, want)
	}
	if got, want := observation.Type, RecordTypeA; got != want {
		t.Fatalf("unexpected record type: got %q want %q", got, want)
	}
	if got, want := observation.Address, destIP; got != want {
		t.Fatalf("unexpected record address: got %s want %s", got, want)
	}
	if got, want := observation.TTL, 30*time.Second; got != want {
		t.Fatalf("unexpected ttl: got %s want %s", got, want)
	}
	if got, want := observation.ExpiresAt, now.Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("unexpected expiry: got %s want %s", got, want)
	}

	allowed := runtime.AllowConnection(Connection{
		SandboxID:  "sandbox-1",
		SourceIP:   sourceIP,
		SourcePort: 40000,
		DestIP:     destIP,
		DestPort:   443,
		Protocol:   ProtocolTCP,
	}, now)
	if !allowed {
		t.Fatal("expected observed allowed destination to be permitted")
	}

	if runtime.AllowConnection(Connection{
		SandboxID:  "sandbox-1",
		SourceIP:   sourceIP,
		SourcePort: 40001,
		DestIP:     destIP,
		DestPort:   80,
		Protocol:   ProtocolTCP,
	}, now) {
		t.Fatal("did not expect disallowed port to be permitted")
	}

	if runtime.AllowConnection(Connection{
		SandboxID:  "sandbox-1",
		SourceIP:   netip.MustParseAddr("10.0.0.3"),
		SourcePort: 40002,
		DestIP:     destIP,
		DestPort:   443,
		Protocol:   ProtocolTCP,
	}, now) {
		t.Fatal("did not expect a different source ip to reuse the observation")
	}
}

func TestRuntimeHonoursMinimumTTLAcrossCNAMEChainAndKeepsEstablishedConnections(t *testing.T) {
	t.Parallel()

	runtime := NewRuntime(RuntimeConfig{
		MaxObservationsPerScope:  8,
		MaxConnectionsPerSandbox: 8,
	})
	if err := runtime.RegisterSandbox("sandbox-1", testCompiledPolicy(
		policy.AllowRule{Host: "service.example", Ports: []int{443}},
	)); err != nil {
		t.Fatalf("register sandbox: %v", err)
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")
	destIP := netip.MustParseAddr("203.0.113.20")

	if err := runtime.ObserveResponse("sandbox-1", sourceIP, testResponse("service.example.",
		&dns.CNAME{
			Hdr:    dns.RR_Header{Name: "service.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 5},
			Target: "edge.example.",
		},
		&dns.CNAME{
			Hdr:    dns.RR_Header{Name: "edge.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 30},
			Target: "cdn.example.",
		},
		&dns.A{
			Hdr: dns.RR_Header{Name: "cdn.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("203.0.113.20"),
		},
	), now); err != nil {
		t.Fatalf("observe response: %v", err)
	}

	observations := runtime.Observations("sandbox-1", now)
	if len(observations) != 3 {
		t.Fatalf("unexpected observation count: got %d want 3", len(observations))
	}

	var addressObservation Observation
	for _, observation := range observations {
		if observation.Type == RecordTypeA {
			addressObservation = observation
			break
		}
	}
	if got, want := addressObservation.Names, []string{"service.example", "edge.example", "cdn.example"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected cname chain: got %v want %v", got, want)
	}
	if got, want := addressObservation.TTL, 5*time.Second; got != want {
		t.Fatalf("unexpected effective ttl: got %s want %s", got, want)
	}
	if got, want := addressObservation.ExpiresAt, now.Add(5*time.Second); !got.Equal(want) {
		t.Fatalf("unexpected effective expiry: got %s want %s", got, want)
	}

	firstFlow := Connection{
		SandboxID:  "sandbox-1",
		SourceIP:   sourceIP,
		SourcePort: 41000,
		DestIP:     destIP,
		DestPort:   443,
		Protocol:   ProtocolTCP,
	}
	if !runtime.AllowConnection(firstFlow, now.Add(4*time.Second)) {
		t.Fatal("expected new connection before expiry to be allowed")
	}

	secondFlow := firstFlow
	secondFlow.SourcePort = 41001
	if runtime.AllowConnection(secondFlow, now.Add(6*time.Second)) {
		t.Fatal("did not expect a new connection after ttl expiry to be allowed")
	}

	if !runtime.AllowConnection(firstFlow, now.Add(6*time.Second)) {
		t.Fatal("expected established connection to survive ttl expiry")
	}

	runtime.ReleaseConnection(firstFlow)
	if runtime.AllowConnection(firstFlow, now.Add(6*time.Second)) {
		t.Fatal("did not expect released connection to survive ttl expiry")
	}
}

func TestRuntimeTreatsZeroAnswerTTLAsImmediateExpiry(t *testing.T) {
	t.Parallel()

	runtime := NewRuntime(RuntimeConfig{
		MaxObservationsPerScope:  8,
		MaxConnectionsPerSandbox: 8,
	})
	if err := runtime.RegisterSandbox("sandbox-1", testCompiledPolicy(
		policy.AllowRule{Host: "service.example", Ports: []int{443}},
	)); err != nil {
		t.Fatalf("register sandbox: %v", err)
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")
	destIP := netip.MustParseAddr("203.0.113.21")

	if err := runtime.ObserveResponse("sandbox-1", sourceIP, testResponse("service.example.",
		&dns.CNAME{
			Hdr:    dns.RR_Header{Name: "service.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 30},
			Target: "cdn.example.",
		},
		&dns.A{
			Hdr: dns.RR_Header{Name: "cdn.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0},
			A:   net.ParseIP("203.0.113.21"),
		},
	), now); err != nil {
		t.Fatalf("observe response: %v", err)
	}

	observations := runtime.Observations("sandbox-1", now)
	if len(observations) != 1 {
		t.Fatalf("unexpected observation count: got %d want 1", len(observations))
	}
	if got, want := observations[0].Type, RecordTypeCNAME; got != want {
		t.Fatalf("unexpected surviving observation type: got %q want %q", got, want)
	}

	for _, observation := range observations {
		if observation.Type == RecordTypeA {
			t.Fatalf("did not expect zero-ttl address observation to remain cached: %+v", observation)
		}
	}

	if runtime.AllowConnection(Connection{
		SandboxID:  "sandbox-1",
		SourceIP:   sourceIP,
		SourcePort: 41010,
		DestIP:     destIP,
		DestPort:   443,
		Protocol:   ProtocolTCP,
	}, now) {
		t.Fatal("did not expect zero-ttl observation to allow new connections")
	}
}

func TestRuntimeIgnoresUnmatchedAnswerOwnersWhenQuestionPresent(t *testing.T) {
	t.Parallel()

	runtime := NewRuntime(RuntimeConfig{
		MaxObservationsPerScope:  8,
		MaxConnectionsPerSandbox: 8,
	})
	if err := runtime.RegisterSandbox("sandbox-1", testCompiledPolicy(
		policy.AllowRule{Host: "cdn.example", Ports: []int{443}},
	)); err != nil {
		t.Fatalf("register sandbox: %v", err)
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")
	destIP := netip.MustParseAddr("203.0.113.22")

	if err := runtime.ObserveResponse("sandbox-1", sourceIP, testResponse("api.example.com.",
		&dns.A{
			Hdr: dns.RR_Header{Name: "cdn.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.ParseIP("203.0.113.22"),
		},
	), now); err != nil {
		t.Fatalf("observe response: %v", err)
	}

	if observations := runtime.Observations("sandbox-1", now); len(observations) != 0 {
		t.Fatalf("did not expect unmatched answer owner to be cached: %+v", observations)
	}

	if runtime.AllowConnection(Connection{
		SandboxID:  "sandbox-1",
		SourceIP:   sourceIP,
		SourcePort: 41011,
		DestIP:     destIP,
		DestPort:   443,
		Protocol:   ProtocolTCP,
	}, now) {
		t.Fatal("did not expect unmatched answer owner to authorize a new connection")
	}
}

func TestRuntimeScopesObservationsBySandboxAndSourceIP(t *testing.T) {
	t.Parallel()

	runtime := NewRuntime(RuntimeConfig{
		MaxObservationsPerScope:  8,
		MaxConnectionsPerSandbox: 8,
	})
	p := testCompiledPolicy(policy.AllowRule{Host: "api.example.com", Ports: []int{443}})
	for _, sandboxID := range []string{"sandbox-a", "sandbox-b"} {
		if err := runtime.RegisterSandbox(sandboxID, p); err != nil {
			t.Fatalf("register sandbox %q: %v", sandboxID, err)
		}
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")
	destIP := netip.MustParseAddr("203.0.113.30")

	if err := runtime.ObserveResponse("sandbox-a", sourceIP, testResponse("api.example.com.",
		&dns.A{
			Hdr: dns.RR_Header{Name: "api.example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.ParseIP("203.0.113.30"),
		},
	), now); err != nil {
		t.Fatalf("observe response: %v", err)
	}

	if runtime.AllowConnection(Connection{
		SandboxID:  "sandbox-b",
		SourceIP:   sourceIP,
		SourcePort: 42000,
		DestIP:     destIP,
		DestPort:   443,
		Protocol:   ProtocolTCP,
	}, now) {
		t.Fatal("did not expect a different sandbox to reuse the observation")
	}

	if runtime.AllowConnection(Connection{
		SandboxID:  "sandbox-a",
		SourceIP:   netip.MustParseAddr("10.0.0.3"),
		SourcePort: 42001,
		DestIP:     destIP,
		DestPort:   443,
		Protocol:   ProtocolTCP,
	}, now) {
		t.Fatal("did not expect a different source ip to reuse the observation")
	}
}

func TestRuntimeEvictsOldestObservationsWhenBounded(t *testing.T) {
	t.Parallel()

	runtime := NewRuntime(RuntimeConfig{
		MaxObservationsPerScope:  2,
		MaxConnectionsPerSandbox: 8,
	})
	if err := runtime.RegisterSandbox("sandbox-1", testCompiledPolicy(
		policy.AllowRule{Host: "one.example", Ports: []int{443}},
		policy.AllowRule{Host: "two.example", Ports: []int{443}},
		policy.AllowRule{Host: "three.example", Ports: []int{443}},
	)); err != nil {
		t.Fatalf("register sandbox: %v", err)
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")

	cases := []struct {
		query string
		ip    string
	}{
		{query: "one.example.", ip: "203.0.113.41"},
		{query: "two.example.", ip: "203.0.113.42"},
		{query: "three.example.", ip: "203.0.113.43"},
	}
	for i, tc := range cases {
		if err := runtime.ObserveResponse("sandbox-1", sourceIP, testResponse(tc.query,
			&dns.A{
				Hdr: dns.RR_Header{Name: tc.query, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
				A:   net.ParseIP(tc.ip),
			},
		), now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("observe response %q: %v", tc.query, err)
		}
	}

	observations := runtime.Observations("sandbox-1", now.Add(3*time.Second))
	if len(observations) != 2 {
		t.Fatalf("unexpected observation count: got %d want 2", len(observations))
	}
	if got, want := observations[0].QueryName, "two.example"; got != want {
		t.Fatalf("unexpected oldest retained query: got %q want %q", got, want)
	}
	if got, want := observations[1].QueryName, "three.example"; got != want {
		t.Fatalf("unexpected newest retained query: got %q want %q", got, want)
	}

	if runtime.AllowConnection(Connection{
		SandboxID:  "sandbox-1",
		SourceIP:   sourceIP,
		SourcePort: 43000,
		DestIP:     netip.MustParseAddr("203.0.113.41"),
		DestPort:   443,
		Protocol:   ProtocolTCP,
	}, now.Add(3*time.Second)) {
		t.Fatal("did not expect evicted observation to keep allowing new connections")
	}

	for _, ip := range []string{"203.0.113.42", "203.0.113.43"} {
		if !runtime.AllowConnection(Connection{
			SandboxID:  "sandbox-1",
			SourceIP:   sourceIP,
			SourcePort: 43001,
			DestIP:     netip.MustParseAddr(ip),
			DestPort:   443,
			Protocol:   ProtocolTCP,
		}, now.Add(3*time.Second)) {
			t.Fatalf("expected retained observation for %s to allow a new connection", ip)
		}
	}
}

func TestForwarderRecordsScopedUpstreamAnswers(t *testing.T) {
	t.Parallel()

	runtime := NewRuntime(RuntimeConfig{
		MaxObservationsPerScope:  8,
		MaxConnectionsPerSandbox: 8,
	})
	if err := runtime.RegisterSandbox("sandbox-1", testCompiledPolicy(
		policy.AllowRule{Host: "api.example.com", Ports: []int{443}},
	)); err != nil {
		t.Fatalf("register sandbox: %v", err)
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")
	destIP := netip.MustParseAddr("203.0.113.44")

	forwarder := NewForwarder(ForwarderConfig{
		Runtime:      runtime,
		UpstreamAddr: "127.0.0.1:5300",
		Now:          func() time.Time { return now },
		ScopeResolver: func(addr netip.Addr) (string, bool) {
			if addr == sourceIP {
				return "sandbox-1", true
			}
			return "", false
		},
		Client: exchangeFunc(func(msg *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
			if got, want := addr, "127.0.0.1:5300"; got != want {
				t.Fatalf("unexpected upstream addr: got %q want %q", got, want)
			}
			if len(msg.Question) != 1 || msg.Question[0].Name != "api.example.com." {
				t.Fatalf("unexpected forwarded question: %+v", msg.Question)
			}
			return testResponse("api.example.com.",
				&dns.A{
					Hdr: dns.RR_Header{Name: "api.example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
					A:   net.ParseIP("203.0.113.44"),
				},
			), 5 * time.Millisecond, nil
		}),
	})

	writer := &testResponseWriter{
		remoteAddr: &net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 53000},
		localAddr:  &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1053},
	}
	request := new(dns.Msg)
	request.SetQuestion("api.example.com.", dns.TypeA)

	forwarder.ServeDNS(writer, request)

	if writer.message == nil {
		t.Fatal("expected forwarder to write a response")
	}
	if len(writer.message.Answer) != 1 {
		t.Fatalf("unexpected forwarded answer count: got %d want 1", len(writer.message.Answer))
	}

	observations := runtime.Observations("sandbox-1", now)
	if len(observations) != 1 {
		t.Fatalf("unexpected observation count: got %d want 1", len(observations))
	}
	if got, want := observations[0].Address, destIP; got != want {
		t.Fatalf("unexpected observed address: got %s want %s", got, want)
	}
}

func TestForwarderDoesNotRecordScopedAnswersWhenWriteFails(t *testing.T) {
	t.Parallel()

	runtime := NewRuntime(RuntimeConfig{
		MaxObservationsPerScope:  8,
		MaxConnectionsPerSandbox: 8,
	})
	if err := runtime.RegisterSandbox("sandbox-1", testCompiledPolicy(
		policy.AllowRule{Host: "api.example.com", Ports: []int{443}},
	)); err != nil {
		t.Fatalf("register sandbox: %v", err)
	}

	now := time.Date(2026, time.April, 6, 10, 0, 0, 0, time.UTC)
	sourceIP := netip.MustParseAddr("10.0.0.2")

	forwarder := NewForwarder(ForwarderConfig{
		Runtime:      runtime,
		UpstreamAddr: "127.0.0.1:5300",
		Now:          func() time.Time { return now },
		ScopeResolver: func(addr netip.Addr) (string, bool) {
			if addr == sourceIP {
				return "sandbox-1", true
			}
			return "", false
		},
		Client: exchangeFunc(func(msg *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
			return testResponse("api.example.com.",
				&dns.A{
					Hdr: dns.RR_Header{Name: "api.example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
					A:   net.ParseIP("203.0.113.44"),
				},
			), 5 * time.Millisecond, nil
		}),
	})

	writer := &testResponseWriter{
		remoteAddr:  &net.UDPAddr{IP: net.ParseIP("10.0.0.2"), Port: 53000},
		localAddr:   &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1053},
		writeMsgErr: errors.New("write failed"),
	}
	request := new(dns.Msg)
	request.SetQuestion("api.example.com.", dns.TypeA)

	forwarder.ServeDNS(writer, request)

	if observations := runtime.Observations("sandbox-1", now); len(observations) != 0 {
		t.Fatalf("did not expect failed response writes to record observations: %+v", observations)
	}
}

func TestAddrFromNetAddrHandlesNilValues(t *testing.T) {
	t.Parallel()

	var nilUDP *net.UDPAddr
	var nilTCP *net.TCPAddr

	cases := []net.Addr{
		nil,
		nilUDP,
		nilTCP,
	}

	for _, addr := range cases {
		if parsed, ok := addrFromNetAddr(addr); ok || parsed.IsValid() {
			t.Fatalf("expected nil addr %T to be ignored, got %v %v", addr, parsed, ok)
		}
	}
}

func testCompiledPolicy(allow ...policy.AllowRule) *policy.CompiledPolicy {
	return &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          allow,
	}
}

func testResponse(query string, answers ...dns.RR) *dns.Msg {
	msg := new(dns.Msg)
	msg.SetQuestion(query, dns.TypeA)
	msg.Response = true
	msg.Answer = append(msg.Answer, answers...)
	return msg
}

type exchangeFunc func(*dns.Msg, string) (*dns.Msg, time.Duration, error)

func (f exchangeFunc) Exchange(msg *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
	return f(msg, addr)
}

type testResponseWriter struct {
	remoteAddr  net.Addr
	localAddr   net.Addr
	message     *dns.Msg
	writeMsgErr error
}

func (w *testResponseWriter) LocalAddr() net.Addr       { return w.localAddr }
func (w *testResponseWriter) RemoteAddr() net.Addr      { return w.remoteAddr }
func (w *testResponseWriter) Close() error              { return nil }
func (w *testResponseWriter) TsigStatus() error         { return nil }
func (w *testResponseWriter) TsigTimersOnly(bool)       {}
func (w *testResponseWriter) Hijack()                   {}
func (w *testResponseWriter) Write([]byte) (int, error) { return 0, nil }
func (w *testResponseWriter) WriteMsg(msg *dns.Msg) error {
	if w.writeMsgErr != nil {
		return w.writeMsgErr
	}
	w.message = msg.Copy()
	return nil
}

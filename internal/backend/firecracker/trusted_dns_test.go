package firecracker

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type testTrustedDNSExchangeFunc func(*dns.Msg, string) (*dns.Msg, time.Duration, error)

func (f testTrustedDNSExchangeFunc) Exchange(msg *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
	return f(msg, addr)
}

func TestTrustedDNSUpstreamAddrsFromConfigUsesAllConfiguredResolvers(t *testing.T) {
	t.Parallel()

	upstreamAddrs, err := trustedDNSUpstreamAddrsFromConfig(&dns.ClientConfig{
		Servers: []string{"1.1.1.1", " ", "8.8.8.8"},
		Port:    "53",
	})
	if err != nil {
		t.Fatalf("trustedDNSUpstreamAddrsFromConfig: %v", err)
	}

	if got, want := strings.Join(upstreamAddrs, ","), "1.1.1.1:53,8.8.8.8:53"; got != want {
		t.Fatalf("unexpected upstream addrs: got %q want %q", got, want)
	}
}

func TestTrustedDNSMultiUpstreamClientFallsBackToLaterResolver(t *testing.T) {
	t.Parallel()

	var calls []string
	client := trustedDNSMultiUpstreamClient{
		upstreamAddrs: []string{"1.1.1.1:53", "8.8.8.8:53"},
		client: testTrustedDNSExchangeFunc(func(msg *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
			calls = append(calls, addr)
			if addr == "1.1.1.1:53" {
				return nil, 0, errors.New("dial udp 1.1.1.1:53: i/o timeout")
			}
			if msg == nil || len(msg.Question) != 1 || msg.Question[0].Name != "api.example.com." {
				t.Fatalf("unexpected forwarded question: %+v", msg)
			}
			return testDNSResponse("api.example.com.",
				&dns.A{
					Hdr: dns.RR_Header{Name: "api.example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
					A:   net.ParseIP("203.0.113.44"),
				},
			), 5 * time.Millisecond, nil
		}),
	}

	req := new(dns.Msg)
	req.SetQuestion("api.example.com.", dns.TypeA)

	resp, _, err := client.Exchange(req, "ignored")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if resp == nil || len(resp.Answer) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if got, want := strings.Join(calls, ","), "1.1.1.1:53,8.8.8.8:53"; got != want {
		t.Fatalf("unexpected upstream attempts: got %q want %q", got, want)
	}
}

package cli

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/endpoint"
)

func TestResolveInteractiveQUICEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		ep                endpoint.Endpoint
		wantListenAddr    string
		wantAdvertiseHost string
	}{
		{
			name: "unix endpoint uses loopback ephemeral listener",
			ep: endpoint.Endpoint{
				Scheme:  "unix",
				Address: "/tmp/cleanroom.sock",
			},
			wantListenAddr:    "127.0.0.1:0",
			wantAdvertiseHost: "127.0.0.1",
		},
		{
			name: "http wildcard listen preserves wildcard advertise host",
			ep: endpoint.Endpoint{
				Scheme:  "http",
				Address: "http://0.0.0.0:7777",
			},
			wantListenAddr:    "0.0.0.0:7777",
			wantAdvertiseHost: "0.0.0.0",
		},
		{
			name: "http explicit host preserves explicit advertise host",
			ep: endpoint.Endpoint{
				Scheme:  "http",
				Address: "http://cleanroom.example:7777",
			},
			wantListenAddr:    "cleanroom.example:7777",
			wantAdvertiseHost: "cleanroom.example",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotListenAddr, gotAdvertiseHost := resolveInteractiveQUICEndpoint(tc.ep)
			if gotListenAddr != tc.wantListenAddr {
				t.Fatalf("listen address mismatch: got %q want %q", gotListenAddr, tc.wantListenAddr)
			}
			if gotAdvertiseHost != tc.wantAdvertiseHost {
				t.Fatalf("advertise host mismatch: got %q want %q", gotAdvertiseHost, tc.wantAdvertiseHost)
			}
		})
	}
}

func TestResolveInteractiveDialEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		controlEP      endpoint.Endpoint
		quicEndpoint   string
		wantDialTarget string
	}{
		{
			name: "remote control host replaces loopback quic endpoint host",
			controlEP: endpoint.Endpoint{
				Scheme:  "http",
				BaseURL: "http://remote.cleanroom.example:7777",
			},
			quicEndpoint:   "127.0.0.1:7777",
			wantDialTarget: "remote.cleanroom.example:7777",
		},
		{
			name: "remote control host replaces wildcard quic endpoint host",
			controlEP: endpoint.Endpoint{
				Scheme:  "http",
				BaseURL: "http://10.0.2.10:7777",
			},
			quicEndpoint:   "0.0.0.0:7777",
			wantDialTarget: "10.0.2.10:7777",
		},
		{
			name: "remote ipv6 control host replaces wildcard ipv6 quic endpoint host",
			controlEP: endpoint.Endpoint{
				Scheme:  "https",
				BaseURL: "https://[2001:db8::5]:8443",
			},
			quicEndpoint:   "[::]:8443",
			wantDialTarget: "[2001:db8::5]:8443",
		},
		{
			name: "unix control endpoint keeps local quic endpoint host",
			controlEP: endpoint.Endpoint{
				Scheme:  "unix",
				Address: "/tmp/cleanroom.sock",
			},
			quicEndpoint:   "127.0.0.1:7777",
			wantDialTarget: "127.0.0.1:7777",
		},
		{
			name: "non-loopback quic endpoint host is preserved",
			controlEP: endpoint.Endpoint{
				Scheme:  "http",
				BaseURL: "http://remote.cleanroom.example:7777",
			},
			quicEndpoint:   "quic.cleanroom.example:7777",
			wantDialTarget: "quic.cleanroom.example:7777",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := resolveInteractiveDialEndpoint(tc.controlEP, tc.quicEndpoint)
			if got != tc.wantDialTarget {
				t.Fatalf("dial endpoint mismatch: got %q want %q", got, tc.wantDialTarget)
			}
		})
	}
}

package dnsproxy

import (
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

// DNSClient exchanges DNS messages with an upstream resolver.
type DNSClient interface {
	Exchange(msg *dns.Msg, addr string) (*dns.Msg, time.Duration, error)
}

// ScopeResolver maps an inbound source IP to a sandbox identity.
type ScopeResolver func(sourceIP netip.Addr) (sandboxID string, ok bool)

// ForwarderConfig configures a DNS forwarding handler.
type ForwarderConfig struct {
	Runtime       *Runtime
	UpstreamAddr  string
	ScopeResolver ScopeResolver
	Client        DNSClient
	Now           func() time.Time
}

// Forwarder forwards DNS requests to an upstream resolver and records the
// answers in the runtime when the caller is mapped to a sandbox scope.
type Forwarder struct {
	runtime       *Runtime
	upstreamAddr  string
	scopeResolver ScopeResolver
	client        DNSClient
	now           func() time.Time
}

// NewForwarder creates a DNS forwarding handler backed by miekg/dns.
func NewForwarder(cfg ForwarderConfig) *Forwarder {
	client := cfg.Client
	if client == nil {
		client = &dns.Client{}
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Forwarder{
		runtime:       cfg.Runtime,
		upstreamAddr:  cfg.UpstreamAddr,
		scopeResolver: cfg.ScopeResolver,
		client:        client,
		now:           now,
	}
}

// ServeDNS implements dns.Handler.
func (f *Forwarder) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	if req == nil || f.client == nil || f.upstreamAddr == "" {
		_ = writeServerFailure(w, req)
		return
	}

	resp, _, err := f.client.Exchange(req.Copy(), f.upstreamAddr)
	if err != nil || resp == nil {
		_ = writeServerFailure(w, req)
		return
	}

	f.observeScopedResponse(w.RemoteAddr(), resp)
	_ = w.WriteMsg(resp)
}

func (f *Forwarder) observeScopedResponse(remoteAddr net.Addr, resp *dns.Msg) {
	if f.runtime == nil || f.scopeResolver == nil || resp == nil {
		return
	}
	sourceIP, ok := addrFromNetAddr(remoteAddr)
	if !ok {
		return
	}
	sandboxID, ok := f.scopeResolver(sourceIP)
	if !ok {
		return
	}
	_ = f.runtime.ObserveResponse(sandboxID, sourceIP, resp.Copy(), f.now().UTC())
}

func addrFromNetAddr(addr net.Addr) (netip.Addr, bool) {
	switch typed := addr.(type) {
	case *net.TCPAddr:
		addr, ok := netip.AddrFromSlice(typed.IP)
		return normalizeAddr(addr), ok
	case *net.UDPAddr:
		addr, ok := netip.AddrFromSlice(typed.IP)
		return normalizeAddr(addr), ok
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return netip.Addr{}, false
		}
		parsed, err := netip.ParseAddr(host)
		if err != nil {
			return netip.Addr{}, false
		}
		return normalizeAddr(parsed), true
	}
}

func writeServerFailure(w dns.ResponseWriter, req *dns.Msg) error {
	msg := new(dns.Msg)
	if req != nil {
		msg.SetRcode(req, dns.RcodeServerFailure)
	} else {
		msg.MsgHdr.Rcode = dns.RcodeServerFailure
		msg.Response = true
	}
	return w.WriteMsg(msg)
}

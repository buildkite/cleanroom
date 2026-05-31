package dnsproxy

import (
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/miekg/dns"
)

// DNSClient exchanges DNS messages with an upstream resolver.
type DNSClient interface {
	Exchange(msg *dns.Msg, addr string) (*dns.Msg, time.Duration, error)
}

// ScopeResolver maps an inbound source IP to a sandbox identity.
type ScopeResolver func(sourceIP netip.Addr) (sandboxID string, ok bool)

// ObserveHook runs after a scoped response has been recorded in the runtime.
type ObserveHook func(sandboxID string, sourceIP netip.Addr)

// DenyHook runs when a DNS response is observed for a host that is not in the
// sandbox's network allow policy.
type DenyHook func(sandboxID, queryName string)

// StaticRecord configures a synthetic DNS record served directly by the
// forwarder without contacting an upstream resolver.
type StaticRecord struct {
	Name      string
	Addresses []netip.Addr
}

// ForwarderConfig configures a DNS forwarding handler.
type ForwarderConfig struct {
	Runtime                *Runtime
	UpstreamAddr           string
	ScopeResolver          ScopeResolver
	Client                 DNSClient
	Now                    func() time.Time
	OnObserve              ObserveHook
	OnDeny                 DenyHook
	StaticRecords          []StaticRecord
	BlockDisallowedQueries bool
}

// Forwarder forwards DNS requests to an upstream resolver and records the
// answers in the runtime when the caller is mapped to a sandbox scope.
type Forwarder struct {
	runtime                *Runtime
	upstreamAddr           string
	scopeResolver          ScopeResolver
	client                 DNSClient
	now                    func() time.Time
	onObserve              ObserveHook
	onDeny                 DenyHook
	staticRecords          map[string][]netip.Addr
	blockDisallowedQueries bool
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
		runtime:                cfg.Runtime,
		upstreamAddr:           cfg.UpstreamAddr,
		scopeResolver:          cfg.ScopeResolver,
		client:                 client,
		now:                    now,
		onObserve:              cfg.OnObserve,
		onDeny:                 cfg.OnDeny,
		staticRecords:          normalizeStaticRecords(cfg.StaticRecords),
		blockDisallowedQueries: cfg.BlockDisallowedQueries,
	}
}

// ServeDNS implements dns.Handler.
func (f *Forwarder) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	if req == nil {
		_ = writeServerFailure(w, req)
		return
	}
	if resp, ok := f.staticResponse(req); ok {
		_ = w.WriteMsg(resp)
		return
	}
	if f.client == nil || f.upstreamAddr == "" {
		_ = writeServerFailure(w, req)
		return
	}
	upstreamReq := req.Copy()
	if f.blockDisallowedQueries {
		questions, ok := f.questionsAllowedByPolicy(w.RemoteAddr(), req)
		if !ok {
			_ = writeRefused(w, req)
			return
		}
		upstreamReq = sanitizedQueryForUpstream(questions, req.Id)
	}
	resp, _, err := f.client.Exchange(upstreamReq, f.upstreamAddr)
	if err != nil || resp == nil {
		_ = writeServerFailure(w, req)
		return
	}
	if f.blockDisallowedQueries {
		resp.Id = req.Id
		resp.Question = append([]dns.Question(nil), req.Question...)
	}

	if err := w.WriteMsg(resp); err != nil {
		return
	}
	f.observeScopedResponse(w.RemoteAddr(), resp)
}

func (f *Forwarder) questionsAllowedByPolicy(remoteAddr net.Addr, req *dns.Msg) ([]dns.Question, bool) {
	if f.runtime == nil || f.scopeResolver == nil || req == nil || len(req.Question) == 0 {
		return nil, false
	}
	sourceIP, ok := addrFromNetAddr(remoteAddr)
	if !ok {
		return nil, false
	}
	sandboxID, ok := f.scopeResolver(sourceIP)
	if !ok {
		return nil, false
	}
	allowAll := f.runtime.SandboxAllowsAllQueries(sandboxID)

	allAllowed := true
	questions := make([]dns.Question, 0, len(req.Question))
	for _, question := range req.Question {
		name := normalizeName(question.Name)
		if name == "" || question.Qclass != dns.ClassINET || (!allowAll && !allowedDNSQuestionType(question.Qtype)) || !f.runtime.HostAllowedByPolicy(sandboxID, name) {
			allAllowed = false
			if f.onDeny != nil && name != "" {
				f.onDeny(sandboxID, name)
			}
			continue
		}
		questions = append(questions, dns.Question{
			Name:   dns.Fqdn(name),
			Qtype:  question.Qtype,
			Qclass: dns.ClassINET,
		})
	}
	if !allAllowed || len(questions) == 0 {
		return nil, false
	}
	return questions, true
}

func allowedDNSQuestionType(qtype uint16) bool {
	switch qtype {
	case dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeHTTPS, dns.TypeSVCB:
		return true
	default:
		return false
	}
}

func sanitizedQueryForUpstream(questions []dns.Question, originalID uint16) *dns.Msg {
	msg := new(dns.Msg)
	msg.Id = upstreamDNSID(originalID)
	msg.MsgHdr.Opcode = dns.OpcodeQuery
	msg.MsgHdr.RecursionDesired = true
	msg.Question = append([]dns.Question(nil), questions...)
	msg.SetEdns0(1232, false)
	return msg
}

func upstreamDNSID(originalID uint16) uint16 {
	for i := 0; i < 4; i++ {
		id := dns.Id()
		if id != 0 && id != originalID {
			return id
		}
	}
	if originalID != 1 {
		return 1
	}
	return 2
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
	now := f.now().UTC()
	if err := f.runtime.ObserveResponse(sandboxID, sourceIP, resp.Copy(), now); err != nil {
		return
	}
	if f.onObserve != nil {
		f.onObserve(sandboxID, sourceIP)
	}
	if f.onDeny != nil {
		for _, question := range resp.Question {
			name := normalizeName(question.Name)
			if name != "" && !f.runtime.queryAllowedByPolicy(sandboxID, sourceIP, resp, name, now) {
				f.onDeny(sandboxID, name)
			}
		}
	}
}

func addrFromNetAddr(addr net.Addr) (netip.Addr, bool) {
	if addr == nil {
		return netip.Addr{}, false
	}

	switch typed := addr.(type) {
	case *net.TCPAddr:
		if typed == nil {
			return netip.Addr{}, false
		}
		addr, ok := netip.AddrFromSlice(typed.IP)
		return normalizeAddr(addr), ok
	case *net.UDPAddr:
		if typed == nil {
			return netip.Addr{}, false
		}
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

func writeRefused(w dns.ResponseWriter, req *dns.Msg) error {
	msg := new(dns.Msg)
	if req != nil {
		msg.SetRcode(req, dns.RcodeRefused)
	} else {
		msg.MsgHdr.Rcode = dns.RcodeRefused
		msg.Response = true
	}
	return w.WriteMsg(msg)
}

func normalizeStaticRecords(records []StaticRecord) map[string][]netip.Addr {
	if len(records) == 0 {
		return nil
	}

	out := make(map[string][]netip.Addr, len(records))
	for _, record := range records {
		name := normalizeName(record.Name)
		if name == "" {
			continue
		}
		for _, addr := range record.Addresses {
			addr = normalizeAddr(addr)
			if !addr.IsValid() {
				continue
			}
			if slices.Contains(out[name], addr) {
				continue
			}
			out[name] = append(out[name], addr)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (f *Forwarder) staticResponse(req *dns.Msg) (*dns.Msg, bool) {
	if req == nil || len(f.staticRecords) == 0 || len(req.Question) == 0 {
		return nil, false
	}

	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true

	matched := false
	for _, question := range req.Question {
		name := normalizeName(question.Name)
		addresses := f.staticRecords[name]
		if len(addresses) == 0 {
			continue
		}
		matched = true
		for _, addr := range addresses {
			switch {
			case question.Qtype == dns.TypeA && addr.Is4():
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.RR_Header{
						Name:   dns.Fqdn(name),
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    60,
					},
					A: net.IP(addr.AsSlice()),
				})
			case question.Qtype == dns.TypeAAAA && addr.Is6():
				resp.Answer = append(resp.Answer, &dns.AAAA{
					Hdr: dns.RR_Header{
						Name:   dns.Fqdn(name),
						Rrtype: dns.TypeAAAA,
						Class:  dns.ClassINET,
						Ttl:    60,
					},
					AAAA: net.IP(addr.AsSlice()),
				})
			case question.Qtype == dns.TypeANY:
				if addr.Is4() {
					resp.Answer = append(resp.Answer, &dns.A{
						Hdr: dns.RR_Header{
							Name:   dns.Fqdn(name),
							Rrtype: dns.TypeA,
							Class:  dns.ClassINET,
							Ttl:    60,
						},
						A: net.IP(addr.AsSlice()),
					})
					continue
				}
				if addr.Is6() {
					resp.Answer = append(resp.Answer, &dns.AAAA{
						Hdr: dns.RR_Header{
							Name:   dns.Fqdn(name),
							Rrtype: dns.TypeAAAA,
							Class:  dns.ClassINET,
							Ttl:    60,
						},
						AAAA: net.IP(addr.AsSlice()),
					})
				}
			}
		}
	}

	if !matched {
		return nil, false
	}
	return resp, true
}

package exposure

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/charmbracelet/log"
	"github.com/miekg/dns"
)

const (
	Domain             = "cleanroom.localhost"
	DefaultDNSListen   = "127.0.0.1:8153"
	DefaultHTTPSListen = "127.0.0.1:8143"
	dnsTakeoverEvery   = 100 * time.Millisecond
)

type Dialer func(ctx context.Context, sandboxID string, port int) (net.Conn, error)

type RegisterRequest struct {
	OwnerID   string
	SandboxID string
	Exposure  *cleanroomv1.PortExposure
	Dialer    Dialer
}

type Config struct {
	Domain             string
	TCPHost            string
	DNSListen          string
	HTTPSListen        string
	AllowHTTPSFallback bool
	TLSDir             string
	Logger             *log.Logger
}

type Manager struct {
	domain      string
	tcpHost     string
	dnsListen   string
	httpsListen string
	fixedHTTPS  bool
	tlsDir      string
	logger      *log.Logger

	mu          sync.RWMutex
	byOwner     map[string][]*route
	tcpRoutes   map[int]*route
	httpsRoutes map[string]*route
	tcpServers  map[int]*tcpServer

	httpsServer *http.Server
	httpsLn     net.Listener

	dnsServers []*dns.Server
	closeOnce  sync.Once
	closed     chan struct{}
}

type route struct {
	ownerID   string
	sandboxID string
	protocol  string
	guestPort int
	hostPort  int
	name      string
	hostname  string
	url       string
	dialer    Dialer

	httpsProxy     *httputil.ReverseProxy
	httpsTransport *http.Transport
}

type tcpServer struct {
	ln     net.Listener
	closed chan struct{}
}

func NewManager(cfg Config) *Manager {
	domain := normalizeDomain(cfg.Domain)
	if domain == "" {
		domain = Domain
	}
	tcpHost := strings.TrimSpace(cfg.TCPHost)
	if tcpHost == "" {
		tcpHost = "127.0.0.1"
	}
	dnsListen := strings.TrimSpace(cfg.DNSListen)
	if dnsListen == "" {
		dnsListen = DefaultDNSListen
	}
	httpsListen := strings.TrimSpace(cfg.HTTPSListen)
	fixedHTTPS := httpsListen != "" && !cfg.AllowHTTPSFallback
	if httpsListen == "" {
		httpsListen = DefaultHTTPSListen
	}
	return &Manager{
		domain:      domain,
		tcpHost:     tcpHost,
		dnsListen:   dnsListen,
		httpsListen: httpsListen,
		fixedHTTPS:  fixedHTTPS,
		tlsDir:      strings.TrimSpace(cfg.TLSDir),
		logger:      cfg.Logger,
		byOwner:     map[string][]*route{},
		tcpRoutes:   map[int]*route{},
		httpsRoutes: map[string]*route{},
		tcpServers:  map[int]*tcpServer{},
		closed:      make(chan struct{}),
	}
}

func (m *Manager) Register(ctx context.Context, req RegisterRequest) (*cleanroomv1.PortExposure, error) {
	if req.Exposure == nil {
		return nil, errors.New("missing exposure")
	}
	ownerID := strings.TrimSpace(req.OwnerID)
	if ownerID == "" {
		return nil, errors.New("missing exposure owner")
	}
	sandboxID := strings.TrimSpace(req.SandboxID)
	if sandboxID == "" {
		return nil, errors.New("missing sandbox id")
	}
	if req.Dialer == nil {
		return nil, errors.New("missing sandbox port dialer")
	}

	switch strings.TrimSpace(req.Exposure.GetProtocol()) {
	case "tcp":
		return m.registerTCP(ctx, ownerID, sandboxID, req.Exposure, req.Dialer)
	case "https":
		return m.registerHTTPS(ctx, ownerID, sandboxID, req.Exposure, req.Dialer)
	default:
		return nil, fmt.Errorf("unsupported exposure protocol %q", req.Exposure.GetProtocol())
	}
}

func (m *Manager) ReleaseOwner(ownerID string) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return
	}

	m.mu.Lock()
	routes := append([]*route(nil), m.byOwner[ownerID]...)
	delete(m.byOwner, ownerID)
	for _, r := range routes {
		switch r.protocol {
		case "tcp":
			delete(m.tcpRoutes, r.hostPort)
			if server := m.tcpServers[r.hostPort]; server != nil {
				delete(m.tcpServers, r.hostPort)
				_ = server.ln.Close()
			}
		case "https":
			delete(m.httpsRoutes, r.hostname)
			closeHTTPSRouteIdleConnections(r)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) StartDNS(ctx context.Context) error {
	if strings.TrimSpace(m.dnsListen) == "" {
		return nil
	}
	m.mu.RLock()
	running := len(m.dnsServers) > 0
	m.mu.RUnlock()
	if running {
		return nil
	}
	if err := m.startDNS(ctx); err != nil {
		if addressInUse(err) && existingDNSAnswers(ctx, m.dnsListen, m.domain) {
			go m.takeOverDNS(ctx)
			return nil
		}
		return err
	}
	return nil
}

func (m *Manager) startDNS(ctx context.Context) error {
	if m.isClosed() {
		return net.ErrClosed
	}
	handler := dns.NewServeMux()
	handler.HandleFunc(dns.Fqdn(m.domain), m.handleDNS)

	udp := &dns.Server{Addr: m.dnsListen, Net: "udp", Handler: handler}
	tcp := &dns.Server{Addr: m.dnsListen, Net: "tcp", Handler: handler}
	if err := startDNSServer(ctx, udp); err != nil {
		return err
	}
	if m.isClosed() {
		_ = udp.Shutdown()
		return net.ErrClosed
	}
	if err := startDNSServer(ctx, tcp); err != nil {
		_ = udp.Shutdown()
		return err
	}
	if m.isClosed() {
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return net.ErrClosed
	}
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return net.ErrClosed
	}
	m.dnsServers = append(m.dnsServers, udp, tcp)
	m.mu.Unlock()
	return nil
}

func (m *Manager) isClosed() bool {
	select {
	case <-m.closed:
		return true
	default:
		return false
	}
}

func (m *Manager) takeOverDNS(ctx context.Context) {
	ticker := time.NewTicker(dnsTakeoverEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.closed:
			return
		case <-ticker.C:
		}
		if existingDNSAnswers(ctx, m.dnsListen, m.domain) {
			continue
		}
		if err := m.startDNS(ctx); err == nil {
			return
		}
	}
}

func (m *Manager) Close() error {
	var errs []error
	m.closeOnce.Do(func() {
		close(m.closed)
	})
	m.mu.Lock()
	for _, server := range m.tcpServers {
		if server != nil {
			if err := server.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
	}
	m.tcpServers = map[int]*tcpServer{}
	for _, routes := range m.byOwner {
		for _, r := range routes {
			closeHTTPSRouteIdleConnections(r)
		}
	}
	m.tcpRoutes = map[int]*route{}
	m.httpsRoutes = map[string]*route{}
	m.byOwner = map[string][]*route{}
	if m.httpsServer != nil {
		if err := m.httpsServer.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	if m.httpsLn != nil {
		if err := m.httpsLn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	dnsServers := append([]*dns.Server(nil), m.dnsServers...)
	m.dnsServers = nil
	m.mu.Unlock()

	for _, server := range dnsServers {
		if server == nil {
			continue
		}
		if err := server.Shutdown(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) registerTCP(ctx context.Context, ownerID, sandboxID string, exposure *cleanroomv1.PortExposure, dialer Dialer) (*cleanroomv1.PortExposure, error) {
	guestPort, err := validPort(exposure.GetGuestPort())
	if err != nil {
		return nil, fmt.Errorf("invalid tcp guest port: %w", err)
	}
	hostPort := int(exposure.GetHostPort())
	if hostPort == 0 {
		hostPort = guestPort
	}
	if _, err := validPort(int32(hostPort)); err != nil {
		return nil, fmt.Errorf("invalid tcp host port: %w", err)
	}

	r := &route{
		ownerID:   ownerID,
		sandboxID: sandboxID,
		protocol:  "tcp",
		guestPort: guestPort,
		hostPort:  hostPort,
		url:       "tcp://" + net.JoinHostPort(m.tcpHost, strconv.Itoa(hostPort)),
		dialer:    dialer,
	}

	m.mu.Lock()
	if _, exists := m.tcpRoutes[hostPort]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("tcp host port %d is already exposed", hostPort)
	}
	m.mu.Unlock()

	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(ctx, "tcp", net.JoinHostPort(m.tcpHost, strconv.Itoa(hostPort)))
	if err != nil {
		return nil, fmt.Errorf("listen tcp exposure on %s: %w", net.JoinHostPort(m.tcpHost, strconv.Itoa(hostPort)), err)
	}
	server := &tcpServer{ln: ln, closed: make(chan struct{})}

	m.mu.Lock()
	if _, exists := m.tcpRoutes[hostPort]; exists {
		m.mu.Unlock()
		_ = ln.Close()
		return nil, fmt.Errorf("tcp host port %d is already exposed", hostPort)
	}
	m.tcpRoutes[hostPort] = r
	m.tcpServers[hostPort] = server
	m.byOwner[ownerID] = append(m.byOwner[ownerID], r)
	m.mu.Unlock()

	go m.serveTCP(server, hostPort)
	return r.toProto(), nil
}

func (m *Manager) registerHTTPS(ctx context.Context, ownerID, sandboxID string, exposure *cleanroomv1.PortExposure, dialer Dialer) (*cleanroomv1.PortExposure, error) {
	guestPort, err := validPort(exposure.GetGuestPort())
	if err != nil {
		return nil, fmt.Errorf("invalid https guest port: %w", err)
	}
	name := strings.TrimSpace(exposure.GetName())
	if name == "" {
		name = sandboxID
	}
	if err := validateDNSLabel(name); err != nil {
		return nil, err
	}
	host := name + "." + m.domain
	r := &route{
		ownerID:   ownerID,
		sandboxID: sandboxID,
		protocol:  "https",
		guestPort: guestPort,
		name:      name,
		hostname:  host,
		dialer:    dialer,
	}
	r.httpsProxy, r.httpsTransport = m.newHTTPSProxy(r)

	m.mu.Lock()
	if _, exists := m.httpsRoutes[host]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("https route %s is already exposed", host)
	}
	m.httpsRoutes[host] = r
	m.byOwner[ownerID] = append(m.byOwner[ownerID], r)
	m.mu.Unlock()

	if err := m.startHTTPS(ctx); err != nil {
		m.releaseRoute(r)
		return nil, err
	}
	m.mu.Lock()
	r.url = m.httpsURL(host)
	m.mu.Unlock()
	return r.toProto(), nil
}

func (m *Manager) releaseRoute(target *route) {
	if target == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch target.protocol {
	case "tcp":
		if m.tcpRoutes[target.hostPort] == target {
			delete(m.tcpRoutes, target.hostPort)
			if server := m.tcpServers[target.hostPort]; server != nil {
				delete(m.tcpServers, target.hostPort)
				_ = server.ln.Close()
			}
		}
	case "https":
		if m.httpsRoutes[target.hostname] == target {
			delete(m.httpsRoutes, target.hostname)
			closeHTTPSRouteIdleConnections(target)
		}
	}
	routes := m.byOwner[target.ownerID]
	for i, r := range routes {
		if r == target {
			routes = append(routes[:i], routes[i+1:]...)
			break
		}
	}
	if len(routes) == 0 {
		delete(m.byOwner, target.ownerID)
		return
	}
	m.byOwner[target.ownerID] = routes
}

func (m *Manager) serveTCP(server *tcpServer, hostPort int) {
	defer close(server.closed)
	for {
		inbound, err := server.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && m.logger != nil {
				m.logger.Warn("tcp exposure accept failed", "host_port", hostPort, "error", err)
			}
			return
		}
		go m.handleTCPConn(inbound, hostPort)
	}
}

func (m *Manager) handleTCPConn(inbound net.Conn, hostPort int) {
	defer inbound.Close()
	m.mu.RLock()
	r := m.tcpRoutes[hostPort]
	m.mu.RUnlock()
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outbound, err := r.dialer(ctx, r.sandboxID, r.guestPort)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("tcp exposure dial failed", "sandbox_id", r.sandboxID, "guest_port", r.guestPort, "host_port", hostPort, "error", err)
		}
		return
	}
	defer outbound.Close()
	errCh := make(chan error, 2)
	go proxyCopyAndCloseWrite(errCh, outbound, inbound)
	go proxyCopyAndCloseWrite(errCh, inbound, outbound)
	<-errCh
	<-errCh
}

func (m *Manager) startHTTPS(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.httpsServer != nil {
		return nil
	}

	cert, err := GenerateServerCertificate(m.domain, m.tlsDir)
	if err != nil {
		return err
	}
	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(ctx, "tcp", m.httpsListen)
	if err != nil {
		if m.fixedHTTPS || !addressInUse(err) {
			return fmt.Errorf("listen https exposure on %s: %w", m.httpsListen, err)
		}
		ln, err = listenConfig.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", "0"))
		if err != nil {
			return fmt.Errorf("listen https exposure on fallback port: %w", err)
		}
	}
	m.httpsListen = ln.Addr().String()
	m.httpsLn = ln
	m.httpsServer = &http.Server{
		Handler:   http.HandlerFunc(m.handleHTTPS),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	go func() {
		err := m.httpsServer.ServeTLS(ln, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && m.logger != nil {
			m.logger.Warn("https exposure server stopped", "error", err)
		}
	}()
	return nil
}

func (m *Manager) newHTTPSProxy(r *route) (*httputil.ReverseProxy, *http.Transport) {
	target := &url.URL{Scheme: "http", Host: "cleanroom-sandbox"}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return r.dialer(ctx, r.sandboxID, r.guestPort)
		},
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if m.logger != nil {
			m.logger.Warn("https exposure proxy failed", "sandbox_id", r.sandboxID, "guest_port", r.guestPort, "hostname", r.hostname, "error", err)
		}
		http.Error(w, "sandbox exposure unavailable", http.StatusBadGateway)
	}
	return proxy, transport
}

func closeHTTPSRouteIdleConnections(r *route) {
	if r != nil && r.httpsTransport != nil {
		r.httpsTransport.CloseIdleConnections()
	}
}

func (m *Manager) handleHTTPS(w http.ResponseWriter, req *http.Request) {
	host := normalizeRequestHost(req.Host)
	if host == "" && req.TLS != nil {
		host = normalizeDomain(req.TLS.ServerName)
	}
	m.mu.RLock()
	r := m.httpsRoutes[host]
	m.mu.RUnlock()
	if r == nil {
		http.NotFound(w, req)
		return
	}
	if r.httpsProxy == nil {
		http.Error(w, "sandbox exposure unavailable", http.StatusBadGateway)
		return
	}
	r.httpsProxy.ServeHTTP(w, req)
}

func (m *Manager) handleDNS(w dns.ResponseWriter, msg *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(msg)
	resp.Authoritative = true
	for _, q := range msg.Question {
		name := normalizeDomain(q.Name)
		if !m.handlesDNSName(name) {
			continue
		}
		if q.Qtype != dns.TypeA {
			continue
		}
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
			A:   net.ParseIP("127.0.0.1"),
		})
	}
	if len(resp.Answer) == 0 && !m.hasKnownDNSQuestion(msg) {
		resp.Rcode = dns.RcodeNameError
	}
	_ = w.WriteMsg(resp)
}

func (m *Manager) hasKnownDNSQuestion(msg *dns.Msg) bool {
	if msg == nil {
		return false
	}
	for _, q := range msg.Question {
		if m.handlesDNSName(normalizeDomain(q.Name)) {
			return true
		}
	}
	return false
}

func (m *Manager) handlesDNSName(name string) bool {
	return name == m.domain || strings.HasSuffix(name, "."+m.domain)
}

func (m *Manager) httpsURL(host string) string {
	_, port, err := net.SplitHostPort(m.httpsListen)
	if err != nil || port == "" || port == "443" {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}

func (r *route) toProto() *cleanroomv1.PortExposure {
	return &cleanroomv1.PortExposure{
		Protocol:  r.protocol,
		GuestPort: int32(r.guestPort),
		HostPort:  int32(r.hostPort),
		Name:      r.name,
		Hostname:  r.hostname,
		Url:       r.url,
	}
}

func startDNSServer(ctx context.Context, server *dns.Server) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
		_ = server.Shutdown()
		return ctx.Err()
	}
	return nil
}

func existingDNSAnswers(ctx context.Context, addr, domain string) bool {
	queryCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn("cleanroom-health."+domain), dns.TypeA)
	resp, _, err := (&dns.Client{Net: "udp"}).ExchangeContext(queryCtx, msg, addr)
	if err != nil || resp == nil || resp.Rcode != dns.RcodeSuccess {
		return false
	}
	for _, answer := range resp.Answer {
		a, ok := answer.(*dns.A)
		if ok && a.A.Equal(net.ParseIP("127.0.0.1")) {
			return true
		}
	}
	return false
}

func addressInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

func proxyCopyAndCloseWrite(errCh chan<- error, dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	if closeWriter, ok := dst.(interface{ CloseWrite() error }); ok {
		if closeErr := closeWriter.CloseWrite(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}
	errCh <- err
}

func validPort(port int32) (int, error) {
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %d out of range 1-65535", port)
	}
	return int(port), nil
}

func validateDNSLabel(label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return errors.New("missing dns label")
	}
	if len(label) > 63 {
		return fmt.Errorf("dns label %q is longer than 63 characters", label)
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return fmt.Errorf("dns label %q cannot start or end with '-'", label)
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("dns label %q must contain only lowercase letters, digits, and '-'", label)
		}
	}
	return nil
}

func normalizeRequestHost(host string) string {
	if strings.Contains(host, ":") {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	return normalizeDomain(host)
}

func normalizeDomain(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}

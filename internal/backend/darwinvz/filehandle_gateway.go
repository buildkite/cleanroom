//go:build darwin

package darwinvz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/dnsproxy"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/charmbracelet/log"
	gvtap "github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/tcpproxy"
	gvtransport "github.com/containers/gvisor-tap-vsock/pkg/transport"
	gvtypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	mdns "github.com/miekg/dns"
	logrus "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	fileHandleGatewaySocketName      = "v.sock"
	fileHandleGatewayDefaultMAC      = "5a:94:ef:e4:0c:dd"
	fileHandleGatewayShutdownTimeout = 2 * time.Second
	fileHandleGatewayMTU             = 1500
	fileHandleGatewayDNSPort         = 53
	fileHandleGatewayDNSFallbackAddr = "1.1.1.1:53"
)

var fileHandleDNSClientConfigFromFile = mdns.ClientConfigFromFile

type fileHandleGatewayConfig struct {
	RunDir          string
	SandboxID       string
	SubnetCIDR      string
	GatewayIP       string
	GatewayPort     int
	GatewayMAC      string
	DNSUpstreamAddr string
	HostGatewayURL  string
	Policy          *policy.CompiledPolicy
}

type fileHandleVirtualNetwork struct {
	stack             *stack.Stack
	networkSwitch     *gvtap.Switch
	dnsUDPConn        net.PacketConn
	dnsTCPLn          net.Listener
	dnsUDPServer      *mdns.Server
	dnsTCPServer      *mdns.Server
	dnsRuntime        *dnsproxy.Runtime
	gatewayHTTPLn     net.Listener
	gatewayHTTPServer *http.Server

	warningMu      sync.RWMutex
	warningHandler func(string)
	warnedDests    map[string]struct{}
}

type fileHandleGateway struct {
	socketPath string
	listener   *net.UnixConn
	network    *fileHandleVirtualNetwork
	bridge     *fileHandleGatewayHTTPBridge
	cancel     context.CancelFunc
	done       chan error
	closeOnce  sync.Once

	restoreDependencyLogs func()
	restoreLogsOnce       sync.Once
}

type fileHandleGatewayHTTPBridge struct {
	reverseProxy *httputil.ReverseProxy

	scopeMu    sync.RWMutex
	scopeToken string
}

var (
	fileHandleGatewayDependencyLogMu     sync.Mutex
	fileHandleGatewayDependencyLogUsers  int
	fileHandleGatewayDependencyLogOutput io.Writer
)

func startFileHandleGateway(ctx context.Context, cfg fileHandleGatewayConfig) (*fileHandleGateway, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.RunDir) == "" {
		return nil, errors.New("file-handle gateway run directory is empty")
	}
	if strings.TrimSpace(cfg.SubnetCIDR) == "" {
		return nil, errors.New("file-handle gateway subnet is empty")
	}
	if strings.TrimSpace(cfg.GatewayIP) == "" {
		gatewayIP, err := defaultFileHandleGatewayIP(cfg.SubnetCIDR)
		if err != nil {
			return nil, err
		}
		cfg.GatewayIP = gatewayIP
	}
	if strings.TrimSpace(cfg.GatewayMAC) == "" {
		cfg.GatewayMAC = fileHandleGatewayDefaultMAC
	}
	if cfg.GatewayPort <= 0 {
		cfg.GatewayPort = gateway.DefaultPort
	}
	dnsRuntime, err := newFileHandleDNSRuntime(cfg.SandboxID, cfg.Policy)
	if err != nil {
		return nil, err
	}
	upstreamAddr, err := resolveFileHandleDNSUpstreamAddr(cfg.DNSUpstreamAddr)
	if err != nil {
		return nil, err
	}

	var bridge *fileHandleGatewayHTTPBridge
	if strings.TrimSpace(cfg.HostGatewayURL) != "" {
		bridge, err = newFileHandleGatewayHTTPBridge(cfg.HostGatewayURL)
		if err != nil {
			return nil, err
		}
	}

	network, err := newFileHandleVirtualNetwork(cfg, upstreamAddr, dnsRuntime, bridge)
	if err != nil {
		return nil, fmt.Errorf("create file-handle virtual network: %w", err)
	}

	socketPath := filepath.Join(cfg.RunDir, fileHandleGatewaySocketName)
	if err := ensureUnixSocketPathFits(socketPath); err != nil {
		_ = network.Close()
		return nil, fmt.Errorf("file-handle gateway socket path %q is too long: %w", socketPath, err)
	}
	endpoint := (&url.URL{Scheme: "unixgram", Path: socketPath}).String()
	listener, err := gvtransport.ListenUnixgram(endpoint)
	if err != nil {
		_ = network.Close()
		return nil, fmt.Errorf("listen for file-handle gateway on %q: %w", socketPath, err)
	}

	gatewayCtx, cancel := context.WithCancel(context.Background())
	restoreDependencyLogs := muteFileHandleGatewayDependencyLogs()
	gateway := &fileHandleGateway{
		socketPath: socketPath,
		listener:   listener,
		network:    network,
		bridge:     bridge,
		cancel:     cancel,
		done:       make(chan error, 1),

		restoreDependencyLogs: restoreDependencyLogs,
	}
	go gateway.run(gatewayCtx)
	return gateway, nil
}

func (g *fileHandleGateway) run(ctx context.Context) {
	defer g.restoreLogsOnce.Do(g.restoreDependencyLogs)
	defer close(g.done)
	conn, err := gvtransport.AcceptVfkit(g.listener)
	if err != nil {
		g.done <- err
		return
	}
	g.done <- g.network.AcceptVfkit(ctx, conn)
}

func (g *fileHandleGateway) SocketPath() string {
	if g == nil {
		return ""
	}
	return g.socketPath
}

func (g *fileHandleGateway) Close() error {
	if g == nil {
		return nil
	}

	var closeErr error
	g.closeOnce.Do(func() {
		g.cancel()
		if g.listener != nil {
			if err := g.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				closeErr = errors.Join(closeErr, fmt.Errorf("close file-handle gateway listener: %w", err))
			}
		}
		if g.network != nil {
			if err := g.network.Close(); err != nil {
				closeErr = errors.Join(closeErr, err)
			}
		}
		select {
		case err, ok := <-g.done:
			if ok && !ignoreFileHandleGatewayRunErr(err) {
				closeErr = errors.Join(closeErr, fmt.Errorf("stop file-handle gateway: %w", err))
			}
		case <-time.After(fileHandleGatewayShutdownTimeout):
			closeErr = errors.Join(closeErr, errors.New("timed out waiting for file-handle gateway to stop"))
		}
		if err := os.Remove(g.socketPath); err != nil && !os.IsNotExist(err) {
			closeErr = errors.Join(closeErr, fmt.Errorf("remove file-handle gateway socket %q: %w", g.socketPath, err))
		}
		g.restoreLogsOnce.Do(g.restoreDependencyLogs)
	})
	return closeErr
}

func (g *fileHandleGateway) SetScopeToken(scopeToken string) {
	if g == nil || g.bridge == nil {
		return
	}
	g.bridge.SetScopeToken(scopeToken)
}

func (g *fileHandleGateway) SetWarningHandler(handler func(string)) {
	if g == nil || g.network == nil {
		return
	}
	g.network.warningMu.Lock()
	g.network.warningHandler = handler
	g.network.warnedDests = make(map[string]struct{})
	g.network.warningMu.Unlock()
}

func newFileHandleDNSRuntime(sandboxID string, compiled *policy.CompiledPolicy) (*dnsproxy.Runtime, error) {
	if compiled == nil {
		return nil, nil
	}
	if strings.TrimSpace(compiled.NetworkDefault) != "deny" {
		return nil, fmt.Errorf("darwin-vz backend requires deny-by-default policy, got %q", compiled.NetworkDefault)
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("file-handle gateway sandbox id is empty")
	}

	runtime := dnsproxy.NewRuntime(dnsproxy.RuntimeConfig{})
	if err := runtime.RegisterSandbox(sandboxID, compiled); err != nil {
		return nil, fmt.Errorf("register file-handle dns sandbox %q: %w", sandboxID, err)
	}
	return runtime, nil
}

func resolveFileHandleDNSUpstreamAddr(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		return configured, nil
	}

	conf, err := fileHandleDNSClientConfigFromFile("/etc/resolv.conf")
	if err == nil && conf != nil {
		port := strings.TrimSpace(conf.Port)
		if port == "" {
			port = "53"
		}
		for _, server := range conf.Servers {
			server = strings.TrimSpace(server)
			if server == "" {
				continue
			}
			return net.JoinHostPort(server, port), nil
		}
	}

	return fileHandleGatewayDNSFallbackAddr, nil
}

func newFileHandleScopeResolver(sandboxID, gatewayIP string) dnsproxy.ScopeResolver {
	sandboxID = strings.TrimSpace(sandboxID)
	gatewayAddr, _ := netip.ParseAddr(strings.TrimSpace(gatewayIP))
	return func(sourceIP netip.Addr) (string, bool) {
		if sandboxID == "" {
			return "", false
		}
		sourceIP = sourceIP.Unmap()
		if !sourceIP.IsValid() || !sourceIP.Is4() {
			return "", false
		}
		if gatewayAddr.IsValid() && sourceIP == gatewayAddr {
			return "", false
		}
		return sandboxID, true
	}
}

func newFileHandleVirtualNetwork(cfg fileHandleGatewayConfig, dnsUpstreamAddr string, dnsRuntime *dnsproxy.Runtime, bridge *fileHandleGatewayHTTPBridge) (*fileHandleVirtualNetwork, error) {
	_, parsedSubnet, err := net.ParseCIDR(cfg.SubnetCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse subnet cidr: %w", err)
	}

	tapEndpoint, err := gvtap.NewLinkEndpoint(false, fileHandleGatewayMTU, cfg.GatewayMAC, cfg.GatewayIP, nil)
	if err != nil {
		return nil, fmt.Errorf("create tap endpoint: %w", err)
	}
	networkSwitch := gvtap.NewSwitch(false, fileHandleGatewayMTU)
	tapEndpoint.Connect(networkSwitch)
	networkSwitch.Connect(tapEndpoint)

	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			arp.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
		},
	})
	if err := s.CreateNIC(1, tapEndpoint); err != nil {
		return nil, errors.New(err.String())
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4()).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		return nil, errors.New(err.String())
	}
	s.SetSpoofing(1, true)
	s.SetPromiscuousMode(1, true)

	subnet, err := tcpip.NewSubnet(tcpip.AddrFromSlice(parsedSubnet.IP), tcpip.MaskFromBytes(parsedSubnet.Mask))
	if err != nil {
		return nil, fmt.Errorf("parse gateway subnet: %w", err)
	}
	s.SetRouteTable([]tcpip.Route{{
		Destination: subnet,
		NIC:         1,
	}})

	udpConn, err := gonet.DialUDP(s, &tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4()),
		Port: fileHandleGatewayDNSPort,
	}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("dial dns udp endpoint: %w", err)
	}
	tcpLn, err := gonet.ListenTCP(s, tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4()),
		Port: fileHandleGatewayDNSPort,
	}, ipv4.ProtocolNumber)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("listen dns tcp endpoint: %w", err)
	}
	dnsForwarder := dnsproxy.NewForwarder(dnsproxy.ForwarderConfig{
		Runtime:       dnsRuntime,
		UpstreamAddr:  dnsUpstreamAddr,
		ScopeResolver: newFileHandleScopeResolver(cfg.SandboxID, cfg.GatewayIP),
	})
	dnsUDPServer := &mdns.Server{
		PacketConn: udpConn,
		Handler:    dnsForwarder,
	}
	dnsTCPServer := &mdns.Server{
		Listener: tcpLn,
		Handler:  dnsForwarder,
	}
	go func() { _ = dnsUDPServer.ActivateAndServe() }()
	go func() { _ = dnsTCPServer.ActivateAndServe() }()

	var gatewayHTTPLn net.Listener
	var gatewayHTTPServer *http.Server
	if bridge != nil {
		gatewayHTTPLn, err = gonet.ListenTCP(s, tcpip.FullAddress{
			NIC:  1,
			Addr: tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4()),
			Port: uint16(cfg.GatewayPort),
		}, ipv4.ProtocolNumber)
		if err != nil {
			_ = udpConn.Close()
			_ = tcpLn.Close()
			return nil, fmt.Errorf("listen file-handle gateway http endpoint: %w", err)
		}
		gatewayHTTPServer = &http.Server{Handler: bridge}
		go func() { _ = gatewayHTTPServer.Serve(gatewayHTTPLn) }()
	}

	network := &fileHandleVirtualNetwork{
		stack:             s,
		networkSwitch:     networkSwitch,
		dnsUDPConn:        udpConn,
		dnsTCPLn:          tcpLn,
		dnsUDPServer:      dnsUDPServer,
		dnsTCPServer:      dnsTCPServer,
		dnsRuntime:        dnsRuntime,
		gatewayHTTPLn:     gatewayHTTPLn,
		gatewayHTTPServer: gatewayHTTPServer,
	}

	tcpForwarder := tcp.NewForwarder(s, 0, 10, func(r *tcp.ForwarderRequest) {
		sourceIP, ok := tcpipAddressAsNetipAddr(r.ID().RemoteAddress)
		if !ok {
			r.Complete(true)
			return
		}
		destIP, ok := tcpipAddressAsNetipAddr(r.ID().LocalAddress)
		if !ok {
			r.Complete(true)
			return
		}
		conn := dnsproxy.Connection{
			SandboxID:  cfg.SandboxID,
			SourceIP:   sourceIP,
			SourcePort: r.ID().RemotePort,
			DestIP:     destIP,
			DestPort:   r.ID().LocalPort,
			Protocol:   dnsproxy.ProtocolTCP,
		}
		if dnsRuntime != nil && !dnsRuntime.AllowConnection(conn, time.Now()) {
			dest := destIP.String()
			if names := dnsRuntime.NamesForAddress(cfg.SandboxID, sourceIP, destIP, time.Now()); len(names) > 0 {
				dest = strings.Join(names, ",")
			}
			msg := fmt.Sprintf("network connection blocked: %s:%d", dest, r.ID().LocalPort)
			network.warningMu.Lock()
			handler := network.warningHandler
			_, alreadyWarned := network.warnedDests[msg]
			if handler != nil && !alreadyWarned {
				network.warnedDests[msg] = struct{}{}
			}
			network.warningMu.Unlock()
			if handler != nil && !alreadyWarned {
				handler(msg)
			}
			logFn := log.Info
			if handler != nil {
				logFn = log.Debug
			}
			logFn("filehandle network connection blocked",
				"sandbox_id", cfg.SandboxID,
				"dest_host", dest,
				"dest_port", r.ID().LocalPort,
				"source_ip", sourceIP.String(),
			)
			r.Complete(true)
			return
		}

		outbound, err := net.Dial("tcp", fmt.Sprintf("%s:%d", destIP.String(), r.ID().LocalPort))
		if err != nil {
			if dnsRuntime != nil {
				dnsRuntime.ReleaseConnection(conn)
			}
			r.Complete(true)
			return
		}

		var wq waiter.Queue
		ep, tcpErr := r.CreateEndpoint(&wq)
		r.Complete(false)
		if tcpErr != nil {
			_ = outbound.Close()
			if dnsRuntime != nil {
				dnsRuntime.ReleaseConnection(conn)
			}
			return
		}

		remote := tcpproxy.DialProxy{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return outbound, nil
			},
		}
		guestConn := gonet.NewTCPConn(&wq, ep)
		remote.HandleConn(guestConn)
		if dnsRuntime != nil {
			dnsRuntime.ReleaseConnection(conn)
		}
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	return network, nil
}

func (n *fileHandleVirtualNetwork) AcceptVfkit(ctx context.Context, conn net.Conn) error {
	return n.networkSwitch.Accept(ctx, conn, gvtypes.VfkitProtocol)
}

func (n *fileHandleVirtualNetwork) Close() error {
	if n == nil {
		return nil
	}

	var closeErr error
	if n.dnsUDPConn != nil {
		if err := n.dnsUDPConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = errors.Join(closeErr, fmt.Errorf("close file-handle dns udp socket: %w", err))
		}
	}
	if n.dnsUDPServer != nil {
		if err := n.dnsUDPServer.Shutdown(); err != nil && !ignoreFileHandleDNSServerShutdownErr(err) {
			closeErr = errors.Join(closeErr, fmt.Errorf("shutdown file-handle dns udp server: %w", err))
		}
	}
	if n.dnsTCPLn != nil {
		if err := n.dnsTCPLn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = errors.Join(closeErr, fmt.Errorf("close file-handle dns tcp listener: %w", err))
		}
	}
	if n.dnsTCPServer != nil {
		if err := n.dnsTCPServer.Shutdown(); err != nil && !ignoreFileHandleDNSServerShutdownErr(err) {
			closeErr = errors.Join(closeErr, fmt.Errorf("shutdown file-handle dns tcp server: %w", err))
		}
	}
	if n.gatewayHTTPServer != nil {
		if err := n.gatewayHTTPServer.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			closeErr = errors.Join(closeErr, fmt.Errorf("close file-handle gateway http server: %w", err))
		}
	}
	if n.gatewayHTTPLn != nil {
		if err := n.gatewayHTTPLn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = errors.Join(closeErr, fmt.Errorf("close file-handle gateway http listener: %w", err))
		}
	}
	return closeErr
}

func ignoreFileHandleDNSServerShutdownErr(err error) bool {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return true
	}
	var dnsErr *mdns.Error
	return errors.As(err, &dnsErr) && dnsErr.Error() == "dns: server not started"
}

func newFileHandleGatewayHTTPBridge(targetURL string) (*fileHandleGatewayHTTPBridge, error) {
	targetURL = strings.TrimSpace(targetURL)
	if targetURL == "" {
		return nil, errors.New("file-handle host gateway url is empty")
	}
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("parse file-handle host gateway url %q: %w", targetURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("file-handle host gateway url %q must include scheme and host", targetURL)
	}

	bridge := &fileHandleGatewayHTTPBridge{}
	reverseProxy := httputil.NewSingleHostReverseProxy(target)
	baseDirector := reverseProxy.Director
	reverseProxy.Director = func(req *http.Request) {
		baseDirector(req)
		req.Header.Del(gateway.ScopeTokenHeader)
		if scopeToken := bridge.scopeTokenValue(); scopeToken != "" {
			req.Header.Set(gateway.ScopeTokenHeader, scopeToken)
		}
	}
	bridge.reverseProxy = reverseProxy
	return bridge, nil
}

func (b *fileHandleGatewayHTTPBridge) SetScopeToken(scopeToken string) {
	if b == nil {
		return
	}
	b.scopeMu.Lock()
	b.scopeToken = strings.TrimSpace(scopeToken)
	b.scopeMu.Unlock()
}

func (b *fileHandleGatewayHTTPBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if b == nil || b.reverseProxy == nil {
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	if b.scopeTokenValue() == "" {
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	b.reverseProxy.ServeHTTP(w, r)
}

func (b *fileHandleGatewayHTTPBridge) scopeTokenValue() string {
	if b == nil {
		return ""
	}
	b.scopeMu.RLock()
	defer b.scopeMu.RUnlock()
	return b.scopeToken
}

func ignoreFileHandleGatewayRunErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection") || strings.Contains(err.Error(), "invalid magic length")
}

func muteFileHandleGatewayDependencyLogs() func() {
	fileHandleGatewayDependencyLogMu.Lock()
	defer fileHandleGatewayDependencyLogMu.Unlock()

	logger := logrus.StandardLogger()
	if fileHandleGatewayDependencyLogUsers == 0 {
		fileHandleGatewayDependencyLogOutput = logger.Out
		logger.SetOutput(io.Discard)
	}
	fileHandleGatewayDependencyLogUsers++

	return func() {
		fileHandleGatewayDependencyLogMu.Lock()
		defer fileHandleGatewayDependencyLogMu.Unlock()

		if fileHandleGatewayDependencyLogUsers == 0 {
			return
		}
		fileHandleGatewayDependencyLogUsers--
		if fileHandleGatewayDependencyLogUsers == 0 {
			logger.SetOutput(fileHandleGatewayDependencyLogOutput)
			fileHandleGatewayDependencyLogOutput = nil
		}
	}
}

func defaultFileHandleGatewayIP(subnetCIDR string) (string, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(subnetCIDR))
	if err != nil {
		return "", fmt.Errorf("parse file-handle subnet %q: %w", subnetCIDR, err)
	}
	addr := prefix.Addr()
	if !addr.Is4() {
		return "", fmt.Errorf("file-handle subnet %q must be IPv4", subnetCIDR)
	}
	base := addr.As4()
	gateway := netip.AddrFrom4([4]byte{base[0], base[1], base[2], base[3] + 1})
	return gateway.String(), nil
}

func tcpipAddressAsNetipAddr(addr tcpip.Address) (netip.Addr, bool) {
	slice := addr.AsSlice()
	if len(slice) == 0 {
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(slice)
	if !ok {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

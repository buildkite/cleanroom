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

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/dnsproxy"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
	gvtap "github.com/containers/gvisor-tap-vsock/pkg/tap"
	gvtransport "github.com/containers/gvisor-tap-vsock/pkg/transport"
	gvtypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/inetaf/tcpproxy"
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

// From the IANA special-purpose address registries, with multicast and reserved
// ranges included so host dials fail closed before policy checks.
var fileHandleGatewayBlockedHostDialDestinationPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("::ffff:0:0/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:3::/32"),
	netip.MustParsePrefix("2001:4:112::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:30::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// From the IANA IPv6 global-unicast assignments. IPv6 destinations outside
// these prefixes are currently reserved or unallocated.
var fileHandleGatewayAllowedIPv6GlobalUnicastPrefixes = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:200::/23"),
	netip.MustParsePrefix("2001:400::/23"),
	netip.MustParsePrefix("2001:600::/23"),
	netip.MustParsePrefix("2001:800::/22"),
	netip.MustParsePrefix("2001:c00::/23"),
	netip.MustParsePrefix("2001:e00::/23"),
	netip.MustParsePrefix("2001:1200::/23"),
	netip.MustParsePrefix("2001:1400::/22"),
	netip.MustParsePrefix("2001:1800::/23"),
	netip.MustParsePrefix("2001:1a00::/23"),
	netip.MustParsePrefix("2001:1c00::/22"),
	netip.MustParsePrefix("2001:2000::/19"),
	netip.MustParsePrefix("2001:4000::/23"),
	netip.MustParsePrefix("2001:4200::/23"),
	netip.MustParsePrefix("2001:4400::/23"),
	netip.MustParsePrefix("2001:4600::/23"),
	netip.MustParsePrefix("2001:4800::/23"),
	netip.MustParsePrefix("2001:4a00::/23"),
	netip.MustParsePrefix("2001:4c00::/23"),
	netip.MustParsePrefix("2001:5000::/20"),
	netip.MustParsePrefix("2001:8000::/19"),
	netip.MustParsePrefix("2001:a000::/20"),
	netip.MustParsePrefix("2001:b000::/20"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2003::/18"),
	netip.MustParsePrefix("2400::/12"),
	netip.MustParsePrefix("2410::/12"),
	netip.MustParsePrefix("2600::/12"),
	netip.MustParsePrefix("2610::/23"),
	netip.MustParsePrefix("2620::/23"),
	netip.MustParsePrefix("2630::/12"),
	netip.MustParsePrefix("2800::/12"),
	netip.MustParsePrefix("2a00::/12"),
	netip.MustParsePrefix("2a10::/12"),
	netip.MustParsePrefix("2c00::/12"),
}

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

	warnings  backend.WarningEmitter
	activeMu  sync.Mutex
	activeTCP map[*fileHandleTCPProxyConn]struct{}
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

type fileHandleTCPProxyConn struct {
	guest    net.Conn
	outbound net.Conn
	cancel   context.CancelFunc
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
	g.network.warnings.SetHandler(handler)
}

func (g *fileHandleGateway) SetPolicy(sandboxID string, compiled *policy.CompiledPolicy) error {
	if g == nil || g.network == nil {
		return nil
	}
	return g.network.SetPolicy(sandboxID, compiled)
}

func (g *fileHandleGateway) DialTCP(ctx context.Context, guestIP string, port int) (net.Conn, error) {
	if g == nil || g.network == nil {
		return nil, errors.New("file-handle gateway is not running")
	}
	return g.network.DialTCP(ctx, guestIP, port)
}

func newFileHandleDNSRuntime(sandboxID string, compiled *policy.CompiledPolicy) (*dnsproxy.Runtime, error) {
	if compiled == nil {
		return nil, nil
	}
	if _, err := evaluateNetworkPolicyForRun(compiled.NetworkDefault, len(compiled.Allow), true); err != nil {
		return nil, err
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
	networkSwitch := gvtap.NewSwitch(false)
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
	staticGatewayAddrs := make([]netip.Addr, 0, 1)
	if gatewayAddr, parseErr := netip.ParseAddr(strings.TrimSpace(cfg.GatewayIP)); parseErr == nil && gatewayAddr.IsValid() {
		staticGatewayAddrs = append(staticGatewayAddrs, gatewayAddr)
	}
	dnsForwarder := dnsproxy.NewForwarder(dnsproxy.ForwarderConfig{
		Runtime:       dnsRuntime,
		UpstreamAddr:  dnsUpstreamAddr,
		ScopeResolver: newFileHandleScopeResolver(cfg.SandboxID, cfg.GatewayIP),
		StaticRecords: []dnsproxy.StaticRecord{{
			Name:      gateway.GuestGatewayHostname,
			Addresses: staticGatewayAddrs,
		}},
		BlockDisallowedQueries: true,
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
		activeTCP:         make(map[*fileHandleTCPProxyConn]struct{}),
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
		if !fileHandleGatewayAllowsHostDialDestination(destIP) {
			msg := fmt.Sprintf("network connection blocked: unsafe destination %s:%d", destIP.String(), r.ID().LocalPort)
			network.warnings.Emit(msg)
			logFn := log.Info
			if network.warnings.HasHandler() {
				logFn = log.Debug
			}
			logFn("filehandle network connection blocked",
				"sandbox_id", cfg.SandboxID,
				"dest_host", destIP.String(),
				"dest_port", r.ID().LocalPort,
				"source_ip", sourceIP.String(),
				"reason", "unsafe host-dial destination",
			)
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
		network.activeMu.Lock()
		if dnsRuntime != nil && !dnsRuntime.AllowConnection(conn, time.Now()) {
			dest := destIP.String()
			if names := dnsRuntime.NamesForAddress(cfg.SandboxID, sourceIP, destIP, time.Now()); len(names) > 0 {
				dest = strings.Join(names, ",")
			}
			msg := fmt.Sprintf("network connection blocked: %s:%d", dest, r.ID().LocalPort)
			network.warnings.Emit(msg)
			logFn := log.Info
			if network.warnings.HasHandler() {
				logFn = log.Debug
			}
			logFn("filehandle network connection blocked",
				"sandbox_id", cfg.SandboxID,
				"dest_host", dest,
				"dest_port", r.ID().LocalPort,
				"source_ip", sourceIP.String(),
			)
			network.activeMu.Unlock()
			r.Complete(true)
			return
		}

		dialCtx, cancelDial := context.WithCancel(context.Background())
		tracked, untrack := network.trackTCPProxyConnLocked(cancelDial)
		network.activeMu.Unlock()
		outboundAddr := net.JoinHostPort(destIP.String(), fmt.Sprint(r.ID().LocalPort))
		outbound, err := new(net.Dialer).DialContext(dialCtx, "tcp", outboundAddr)
		if err != nil {
			untrack()
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
			untrack()
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
		if !network.activateTCPProxyConn(tracked, guestConn, outbound) {
			untrack()
			_ = guestConn.Close()
			_ = outbound.Close()
			if dnsRuntime != nil {
				dnsRuntime.ReleaseConnection(conn)
			}
			return
		}
		defer untrack()
		remote.HandleConn(guestConn)
		if dnsRuntime != nil {
			dnsRuntime.ReleaseConnection(conn)
		}
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	return network, nil
}

func fileHandleGatewayAllowsHostDialDestination(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, prefix := range fileHandleGatewayBlockedHostDialDestinationPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	if addr.Is6() && !fileHandleGatewayAllowsIPv6GlobalUnicastDestination(addr) {
		return false
	}
	return true
}

func fileHandleGatewayAllowsIPv6GlobalUnicastDestination(addr netip.Addr) bool {
	for _, prefix := range fileHandleGatewayAllowedIPv6GlobalUnicastPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (n *fileHandleVirtualNetwork) AcceptVfkit(ctx context.Context, conn net.Conn) error {
	return n.networkSwitch.Accept(ctx, conn, gvtypes.VfkitProtocol)
}

func (n *fileHandleVirtualNetwork) SetPolicy(sandboxID string, compiled *policy.CompiledPolicy) error {
	if n == nil || n.dnsRuntime == nil {
		return nil
	}
	n.activeMu.Lock()
	conns := n.drainActiveTCPProxyConnsLocked()
	closeTCPProxyConns(conns)
	err := n.dnsRuntime.UpdateSandboxPolicy(sandboxID, compiled)
	n.activeMu.Unlock()
	return err
}

func (n *fileHandleVirtualNetwork) trackTCPProxyConn(guest, outbound net.Conn) func() {
	if n == nil {
		return func() {}
	}
	n.activeMu.Lock()
	tracked, untrack := n.trackTCPProxyConnLocked(nil)
	tracked.guest = guest
	tracked.outbound = outbound
	n.activeMu.Unlock()
	return untrack
}

func (n *fileHandleVirtualNetwork) trackTCPProxyConnLocked(cancel context.CancelFunc) (*fileHandleTCPProxyConn, func()) {
	tracked := &fileHandleTCPProxyConn{cancel: cancel}
	if n.activeTCP == nil {
		n.activeTCP = make(map[*fileHandleTCPProxyConn]struct{})
	}
	n.activeTCP[tracked] = struct{}{}
	untrack := func() {
		if cancel != nil {
			cancel()
		}
		n.activeMu.Lock()
		delete(n.activeTCP, tracked)
		n.activeMu.Unlock()
	}
	return tracked, untrack
}

func (n *fileHandleVirtualNetwork) activateTCPProxyConn(tracked *fileHandleTCPProxyConn, guest, outbound net.Conn) bool {
	if n == nil || tracked == nil {
		return false
	}
	n.activeMu.Lock()
	defer n.activeMu.Unlock()
	if _, ok := n.activeTCP[tracked]; !ok {
		return false
	}
	tracked.guest = guest
	tracked.outbound = outbound
	return true
}

func (n *fileHandleVirtualNetwork) closeActiveTCPProxyConns() {
	if n == nil {
		return
	}
	n.activeMu.Lock()
	conns := n.drainActiveTCPProxyConnsLocked()
	n.activeMu.Unlock()
	closeTCPProxyConns(conns)
}

func (n *fileHandleVirtualNetwork) drainActiveTCPProxyConnsLocked() []*fileHandleTCPProxyConn {
	conns := make([]*fileHandleTCPProxyConn, 0, len(n.activeTCP))
	for conn := range n.activeTCP {
		conns = append(conns, conn)
	}
	n.activeTCP = make(map[*fileHandleTCPProxyConn]struct{})
	return conns
}

func closeTCPProxyConns(conns []*fileHandleTCPProxyConn) {
	for _, conn := range conns {
		if conn.cancel != nil {
			conn.cancel()
		}
		if conn.guest != nil {
			_ = conn.guest.Close()
		}
		if conn.outbound != nil {
			_ = conn.outbound.Close()
		}
	}
}

func (n *fileHandleVirtualNetwork) DialTCP(ctx context.Context, guestIP string, port int) (net.Conn, error) {
	if n == nil || n.stack == nil {
		return nil, errors.New("file-handle virtual network is not running")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d out of range 1-65535", port)
	}
	ip := net.ParseIP(strings.TrimSpace(guestIP)).To4()
	if ip == nil {
		return nil, fmt.Errorf("invalid guest ip %q", guestIP)
	}
	return gonet.DialContextTCP(ctx, n.stack, tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4Slice(ip),
		Port: uint16(port),
	}, ipv4.ProtocolNumber)
}

func (n *fileHandleVirtualNetwork) Close() error {
	if n == nil {
		return nil
	}
	n.closeActiveTCPProxyConns()

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

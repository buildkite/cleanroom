//go:build darwin

package darwinvz

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
	gvdns "github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	gvtap "github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/tcpproxy"
	gvtransport "github.com/containers/gvisor-tap-vsock/pkg/transport"
	gvtypes "github.com/containers/gvisor-tap-vsock/pkg/types"
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
)

type fileHandleIPLookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

var defaultFileHandleIPLookup fileHandleIPLookupFunc = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

type fileHandleGatewayConfig struct {
	RunDir         string
	SubnetCIDR     string
	GatewayIP      string
	GatewayPort    int
	GatewayMAC     string
	HostGatewayURL string
	Policy         *policy.CompiledPolicy
	LookupIP       fileHandleIPLookupFunc
}

type fileHandleGatewayPolicy struct {
	allowAll   bool
	allowedTCP map[netip.Addr]map[uint16]struct{}
}

type fileHandleVirtualNetwork struct {
	stack             *stack.Stack
	networkSwitch     *gvtap.Switch
	dnsUDPConn        net.PacketConn
	dnsTCPLn          net.Listener
	gatewayHTTPLn     net.Listener
	gatewayHTTPServer *http.Server
}

type fileHandleGateway struct {
	socketPath string
	listener   *net.UnixConn
	network    *fileHandleVirtualNetwork
	bridge     *fileHandleGatewayHTTPBridge
	cancel     context.CancelFunc
	done       chan error
	closeOnce  sync.Once
}

type fileHandleGatewayHTTPBridge struct {
	reverseProxy *httputil.ReverseProxy

	scopeMu    sync.RWMutex
	scopeToken string
}

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

	lookup := cfg.LookupIP
	if lookup == nil {
		lookup = defaultFileHandleIPLookup
	}
	policyConfig, err := buildFileHandleGatewayPolicy(ctx, cfg.Policy, lookup)
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

	network, err := newFileHandleVirtualNetwork(cfg, policyConfig, bridge)
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
	gateway := &fileHandleGateway{
		socketPath: socketPath,
		listener:   listener,
		network:    network,
		bridge:     bridge,
		cancel:     cancel,
		done:       make(chan error, 1),
	}
	go gateway.run(gatewayCtx)
	return gateway, nil
}

func (g *fileHandleGateway) run(ctx context.Context) {
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
	})
	return closeErr
}

func (g *fileHandleGateway) SetScopeToken(scopeToken string) {
	if g == nil || g.bridge == nil {
		return
	}
	g.bridge.SetScopeToken(scopeToken)
}

func buildFileHandleGatewayPolicy(ctx context.Context, compiled *policy.CompiledPolicy, lookup fileHandleIPLookupFunc) (fileHandleGatewayPolicy, error) {
	if compiled == nil {
		return fileHandleGatewayPolicy{allowAll: true}, nil
	}
	if strings.TrimSpace(compiled.NetworkDefault) != "deny" {
		return fileHandleGatewayPolicy{}, fmt.Errorf("darwin-vz backend requires deny-by-default policy, got %q", compiled.NetworkDefault)
	}
	if lookup == nil {
		lookup = defaultFileHandleIPLookup
	}

	allowedTCP := make(map[netip.Addr]map[uint16]struct{})
	for _, entry := range compiled.Allow {
		host := strings.TrimSpace(strings.ToLower(entry.Host))
		if host == "" || len(entry.Ports) == 0 {
			continue
		}

		var resolvedIPs []netip.Addr
		if addr, err := netip.ParseAddr(host); err == nil {
			if addr.Is4() {
				resolvedIPs = []netip.Addr{addr}
			}
		} else {
			addrs, err := lookup(ctx, host)
			if err != nil {
				return fileHandleGatewayPolicy{}, fmt.Errorf("resolve policy host %q: %w", entry.Host, err)
			}
			for _, addr := range addrs {
				if addr.Is4() {
					resolvedIPs = append(resolvedIPs, addr)
				}
			}
		}
		if len(resolvedIPs) == 0 {
			return fileHandleGatewayPolicy{}, fmt.Errorf("resolve policy host %q: no ipv4 addresses", entry.Host)
		}

		for _, addr := range resolvedIPs {
			ports := allowedTCP[addr]
			if ports == nil {
				ports = make(map[uint16]struct{}, len(entry.Ports))
				allowedTCP[addr] = ports
			}
			for _, port := range entry.Ports {
				if port < 1 || port > 65535 {
					continue
				}
				ports[uint16(port)] = struct{}{}
			}
		}
	}

	return fileHandleGatewayPolicy{allowedTCP: allowedTCP}, nil
}

func (p fileHandleGatewayPolicy) allowsTCP(destIP netip.Addr, destPort uint16) bool {
	if p.allowAll {
		return true
	}
	if !destIP.IsValid() || !destIP.Is4() {
		return false
	}
	ports := p.allowedTCP[destIP]
	if len(ports) == 0 {
		return false
	}
	_, ok := ports[destPort]
	return ok
}

func newFileHandleVirtualNetwork(cfg fileHandleGatewayConfig, policyConfig fileHandleGatewayPolicy, bridge *fileHandleGatewayHTTPBridge) (*fileHandleVirtualNetwork, error) {
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
	dnsServer, err := gvdns.New(udpConn, tcpLn, nil)
	if err != nil {
		_ = udpConn.Close()
		_ = tcpLn.Close()
		return nil, fmt.Errorf("create dns server: %w", err)
	}
	go func() { _ = dnsServer.Serve() }()
	go func() { _ = dnsServer.ServeTCP() }()

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

	tcpForwarder := tcp.NewForwarder(s, 0, 10, func(r *tcp.ForwarderRequest) {
		destIP, ok := tcpipAddressAsNetipAddr(r.ID().LocalAddress)
		if !ok || !policyConfig.allowsTCP(destIP, r.ID().LocalPort) {
			r.Complete(true)
			return
		}

		outbound, err := net.Dial("tcp", fmt.Sprintf("%s:%d", destIP.String(), r.ID().LocalPort))
		if err != nil {
			r.Complete(true)
			return
		}

		var wq waiter.Queue
		ep, tcpErr := r.CreateEndpoint(&wq)
		r.Complete(false)
		if tcpErr != nil {
			_ = outbound.Close()
			return
		}

		remote := tcpproxy.DialProxy{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return outbound, nil
			},
		}
		remote.HandleConn(gonet.NewTCPConn(&wq, ep))
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	return &fileHandleVirtualNetwork{
		stack:             s,
		networkSwitch:     networkSwitch,
		dnsUDPConn:        udpConn,
		dnsTCPLn:          tcpLn,
		gatewayHTTPLn:     gatewayHTTPLn,
		gatewayHTTPServer: gatewayHTTPServer,
	}, nil
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
	if n.dnsTCPLn != nil {
		if err := n.dnsTCPLn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = errors.Join(closeErr, fmt.Errorf("close file-handle dns tcp listener: %w", err))
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

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/dnsproxy"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/miekg/dns"
)

const trustedDNSListenPort = 1053
const trustedDNSSyncRetryDelay = time.Second

type trustedDNSFactory func(context.Context, trustedDNSConfig) (func(), error)

type trustedDNSConfig struct {
	sandboxID    string
	hostIP       netip.Addr
	guestIP      netip.Addr
	policy       *policy.CompiledPolicy
	runBatch     rootCommandBatchFunc
	tcpChainName string
	udpChainName string
	now          func() time.Time
	onDeny       func(sandboxID, queryName string)
}

type trustedDNSChainSyncer struct {
	sandboxID    string
	sourceIP     netip.Addr
	runtime      *dnsproxy.Runtime
	tcpChainName string
	udpChainName string
	runBatch     rootCommandBatchFunc
	now          func() time.Time
}

type trustedDNSChainManager struct {
	sandboxID string
	syncer    *trustedDNSChainSyncer
	trigger   chan struct{}
	done      chan struct{}
}

func trustedDNSTCPChainName(tapName string) string {
	return "crdns-tcp-" + tapName
}

func trustedDNSUDPChainName(tapName string) string {
	return "crdns-udp-" + tapName
}

func newTrustedDNSService(_ context.Context, cfg trustedDNSConfig) (func(), error) {
	if strings.TrimSpace(cfg.sandboxID) == "" {
		return nil, errors.New("trusted dns sandbox id is required")
	}
	if !cfg.hostIP.IsValid() {
		return nil, errors.New("trusted dns host ip must be valid")
	}
	if !cfg.guestIP.IsValid() {
		return nil, errors.New("trusted dns guest ip must be valid")
	}
	if cfg.policy == nil {
		return nil, errors.New("trusted dns policy is required")
	}

	runtime := dnsproxy.NewRuntime(dnsproxy.RuntimeConfig{})
	if err := runtime.RegisterSandbox(cfg.sandboxID, cfg.policy); err != nil {
		return nil, fmt.Errorf("register trusted dns sandbox: %w", err)
	}

	upstreamAddrs, err := trustedDNSUpstreamAddrs()
	if err != nil {
		runtime.ClearSandbox(cfg.sandboxID)
		return nil, err
	}

	var chainManager *trustedDNSChainManager
	if cfg.tcpChainName != "" || cfg.udpChainName != "" {
		if cfg.runBatch == nil {
			runtime.ClearSandbox(cfg.sandboxID)
			return nil, errors.New("trusted dns batch runner is required")
		}
		chainManager = newTrustedDNSChainManager(cfg.sandboxID, &trustedDNSChainSyncer{
			sandboxID:    cfg.sandboxID,
			sourceIP:     cfg.guestIP,
			runtime:      runtime,
			tcpChainName: cfg.tcpChainName,
			udpChainName: cfg.udpChainName,
			runBatch:     cfg.runBatch,
			now:          cfg.now,
		})
	}

	forwarder := dnsproxy.NewForwarder(dnsproxy.ForwarderConfig{
		Runtime:      runtime,
		UpstreamAddr: upstreamAddrs[0],
		Client: trustedDNSMultiUpstreamClient{
			upstreamAddrs: upstreamAddrs,
			client:        &dns.Client{},
		},
		ScopeResolver: func(sourceIP netip.Addr) (string, bool) {
			if sourceIP == cfg.guestIP {
				return cfg.sandboxID, true
			}
			return "", false
		},
		OnObserve: func(string, netip.Addr) {
			if chainManager == nil {
				return
			}
			chainManager.Trigger()
		},
		OnDeny: cfg.onDeny,
	})

	listenAddr := net.JoinHostPort(cfg.hostIP.String(), strconv.Itoa(trustedDNSListenPort))
	udpConn, err := net.ListenPacket("udp4", listenAddr)
	if err != nil {
		runtime.ClearSandbox(cfg.sandboxID)
		return nil, fmt.Errorf("listen for trusted dns udp on %s: %w", listenAddr, err)
	}
	tcpListener, err := net.Listen("tcp4", listenAddr)
	if err != nil {
		_ = udpConn.Close()
		runtime.ClearSandbox(cfg.sandboxID)
		return nil, fmt.Errorf("listen for trusted dns tcp on %s: %w", listenAddr, err)
	}

	udpServer := &dns.Server{PacketConn: udpConn, Handler: forwarder}
	tcpServer := &dns.Server{Listener: tcpListener, Handler: forwarder}

	stopChainManager := func() {}
	if chainManager != nil {
		stopChainManager = chainManager.Start()
	}

	go func() {
		if err := udpServer.ActivateAndServe(); err != nil && !isTrustedDNSServerClosedError(err) {
			log.Printf("trusted dns udp server failed for %s: %v", cfg.sandboxID, err)
		}
	}()
	go func() {
		if err := tcpServer.ActivateAndServe(); err != nil && !isTrustedDNSServerClosedError(err) {
			log.Printf("trusted dns tcp server failed for %s: %v", cfg.sandboxID, err)
		}
	}()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			stopChainManager()
			_ = udpServer.Shutdown()
			_ = tcpServer.Shutdown()
			runtime.ClearSandbox(cfg.sandboxID)
		})
	}
	return cleanup, nil
}

func trustedDNSUpstreamAddrs() ([]string, error) {
	clientCfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("read host dns resolver config: %w", err)
	}
	return trustedDNSUpstreamAddrsFromConfig(clientCfg)
}

func trustedDNSUpstreamAddrsFromConfig(clientCfg *dns.ClientConfig) ([]string, error) {
	if clientCfg == nil {
		return nil, errors.New("read host dns resolver config: no nameservers configured")
	}

	port := strings.TrimSpace(clientCfg.Port)
	if port == "" {
		port = "53"
	}

	upstreamAddrs := make([]string, 0, len(clientCfg.Servers))
	for _, server := range clientCfg.Servers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		upstreamAddrs = append(upstreamAddrs, net.JoinHostPort(server, port))
	}
	if len(upstreamAddrs) == 0 {
		return nil, errors.New("read host dns resolver config: no nameservers configured")
	}
	return upstreamAddrs, nil
}

func newTrustedDNSChainManager(sandboxID string, syncer *trustedDNSChainSyncer) *trustedDNSChainManager {
	return &trustedDNSChainManager{
		sandboxID: sandboxID,
		syncer:    syncer,
		trigger:   make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

func (m *trustedDNSChainManager) Start() func() {
	ctx, cancel := context.WithCancel(context.Background())
	go m.run(ctx)

	return func() {
		cancel()
		<-m.done
	}
}

func (m *trustedDNSChainManager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *trustedDNSChainManager) run(ctx context.Context) {
	defer close(m.done)

	var timer *time.Timer
	var timerCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			stopTrustedDNSTimer(timer)
			return
		case <-m.trigger:
		case <-timerCh:
		}

		nextSync, hasNextSync, err := m.syncOnce()
		if err != nil {
			log.Printf("trusted dns sync failed for %s: %v", m.sandboxID, err)
			nextSync = time.Now().Add(trustedDNSSyncRetryDelay)
			hasNextSync = true
		}

		stopTrustedDNSTimer(timer)
		timer = nil
		timerCh = nil

		if !hasNextSync {
			continue
		}

		delay := time.Until(nextSync)
		if delay < 0 {
			delay = 0
		}
		timer = time.NewTimer(delay)
		timerCh = timer.C
	}
}

func (m *trustedDNSChainManager) syncOnce() (time.Time, bool, error) {
	syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.syncer.Sync(syncCtx)
}

func stopTrustedDNSTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (s trustedDNSChainSyncer) Sync(ctx context.Context) (time.Time, bool, error) {
	if s.runtime == nil {
		return time.Time{}, false, errors.New("trusted dns runtime is required")
	}
	if s.runBatch == nil {
		return time.Time{}, false, errors.New("trusted dns batch runner is required")
	}

	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}

	commands := make([][]string, 0, 2)
	if s.tcpChainName != "" {
		commands = append(commands, []string{"iptables", "-F", s.tcpChainName})
	}
	if s.udpChainName != "" {
		commands = append(commands, []string{"iptables", "-F", s.udpChainName})
	}

	var nextSync time.Time
	hasNextSync := false
	for _, destination := range s.runtime.AllowedDestinations(s.sandboxID, s.sourceIP, now) {
		if !destination.Address.Is4() {
			continue
		}
		if !hasNextSync || destination.ExpiresAt.Before(nextSync) {
			nextSync = destination.ExpiresAt
			hasNextSync = true
		}

		chainName := s.tcpChainName
		switch destination.Protocol {
		case dnsproxy.ProtocolUDP:
			chainName = s.udpChainName
		case dnsproxy.ProtocolTCP:
			chainName = s.tcpChainName
		default:
			continue
		}
		if chainName == "" {
			continue
		}

		commands = append(commands, []string{
			"iptables",
			"-A",
			chainName,
			"-d",
			destination.Address.String(),
			"-p",
			destination.Protocol,
			"--dport",
			destination.Port,
			"-j",
			"ACCEPT",
		})
	}
	if len(commands) == 0 {
		return nextSync, hasNextSync, nil
	}
	if err := s.runBatch(ctx, commands); err != nil {
		return time.Time{}, false, fmt.Errorf("sync trusted dns chains: %w", err)
	}
	return nextSync, hasNextSync, nil
}

func isTrustedDNSServerClosedError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "server shutdown") || strings.Contains(message, "server closed")
}

type trustedDNSMultiUpstreamClient struct {
	upstreamAddrs []string
	client        dnsproxy.DNSClient
}

func (c trustedDNSMultiUpstreamClient) Exchange(msg *dns.Msg, _ string) (*dns.Msg, time.Duration, error) {
	if len(c.upstreamAddrs) == 0 {
		return nil, 0, errors.New("trusted dns upstream resolvers are required")
	}
	if c.client == nil {
		return nil, 0, errors.New("trusted dns client is required")
	}

	var failures []string
	for _, upstreamAddr := range c.upstreamAddrs {
		req := msg
		if msg != nil {
			req = msg.Copy()
		}

		resp, rtt, err := c.client.Exchange(req, upstreamAddr)
		if err == nil && resp != nil {
			return resp, rtt, nil
		}

		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", upstreamAddr, err))
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: nil response", upstreamAddr))
	}

	return nil, 0, fmt.Errorf("exchange with trusted dns upstream resolvers: %s", strings.Join(failures, "; "))
}

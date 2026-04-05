package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
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

type trustedDNSFactory func(context.Context, trustedDNSConfig) (func(), error)

type trustedDNSConfig struct {
	sandboxID  string
	hostIP     netip.Addr
	guestIP    netip.Addr
	policy     *policy.CompiledPolicy
	runBatch   rootCommandBatchFunc
	tcpSetName string
	udpSetName string
	now        func() time.Time
}

type trustedDNSSetSyncer struct {
	sandboxID  string
	sourceIP   netip.Addr
	runtime    *dnsproxy.Runtime
	tcpSetName string
	udpSetName string
	runBatch   rootCommandBatchFunc
	now        func() time.Time
}

func trustedDNSTCPSetName(tapName string) string {
	return "crdns-tcp-" + tapName
}

func trustedDNSUDPSetName(tapName string) string {
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

	upstreamAddr, err := trustedDNSUpstreamAddr()
	if err != nil {
		runtime.ClearSandbox(cfg.sandboxID)
		return nil, err
	}

	var syncer *trustedDNSSetSyncer
	if cfg.tcpSetName != "" || cfg.udpSetName != "" {
		if cfg.runBatch == nil {
			runtime.ClearSandbox(cfg.sandboxID)
			return nil, errors.New("trusted dns batch runner is required")
		}
		syncer = &trustedDNSSetSyncer{
			sandboxID:  cfg.sandboxID,
			sourceIP:   cfg.guestIP,
			runtime:    runtime,
			tcpSetName: cfg.tcpSetName,
			udpSetName: cfg.udpSetName,
			runBatch:   cfg.runBatch,
			now:        cfg.now,
		}
	}

	forwarder := dnsproxy.NewForwarder(dnsproxy.ForwarderConfig{
		Runtime:      runtime,
		UpstreamAddr: upstreamAddr,
		ScopeResolver: func(sourceIP netip.Addr) (string, bool) {
			if sourceIP == cfg.guestIP {
				return cfg.sandboxID, true
			}
			return "", false
		},
		OnObserve: func(string, netip.Addr) {
			if syncer == nil {
				return
			}
			syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := syncer.Sync(syncCtx); err != nil {
				log.Printf("trusted dns sync failed for %s: %v", cfg.sandboxID, err)
			}
		},
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
			_ = udpServer.Shutdown()
			_ = tcpServer.Shutdown()
			runtime.ClearSandbox(cfg.sandboxID)
		})
	}
	return cleanup, nil
}

func trustedDNSUpstreamAddr() (string, error) {
	clientCfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return "", fmt.Errorf("read host dns resolver config: %w", err)
	}
	if len(clientCfg.Servers) == 0 {
		return "", errors.New("read host dns resolver config: no nameservers configured")
	}

	server := strings.TrimSpace(clientCfg.Servers[0])
	if server == "" {
		return "", errors.New("read host dns resolver config: first nameserver is empty")
	}
	port := strings.TrimSpace(clientCfg.Port)
	if port == "" {
		port = "53"
	}
	return net.JoinHostPort(server, port), nil
}

func (s trustedDNSSetSyncer) Sync(ctx context.Context) error {
	if s.runtime == nil {
		return errors.New("trusted dns runtime is required")
	}
	if s.runBatch == nil {
		return errors.New("trusted dns batch runner is required")
	}

	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}

	commands := [][]string{
		{"ipset", "flush", s.tcpSetName},
		{"ipset", "flush", s.udpSetName},
	}
	for _, destination := range s.runtime.AllowedDestinations(s.sandboxID, s.sourceIP, now) {
		if !destination.Address.Is4() {
			continue
		}
		timeout := trustedDNSDestinationTimeoutSeconds(now, destination.ExpiresAt)
		if timeout <= 0 {
			continue
		}

		setName := s.tcpSetName
		switch destination.Protocol {
		case dnsproxy.ProtocolUDP:
			setName = s.udpSetName
		case dnsproxy.ProtocolTCP:
			setName = s.tcpSetName
		default:
			continue
		}
		if setName == "" {
			continue
		}

		commands = append(commands, []string{
			"ipset",
			"add",
			setName,
			destination.Address.String() + "," + destination.Protocol + ":" + destination.Port,
			"timeout",
			strconv.FormatInt(timeout, 10),
		})
	}
	if err := s.runBatch(ctx, commands); err != nil {
		return fmt.Errorf("sync trusted dns ipsets: %w", err)
	}
	return nil
}

func trustedDNSDestinationTimeoutSeconds(now, expiresAt time.Time) int64 {
	return int64(math.Ceil(expiresAt.Sub(now).Seconds()))
}

func isTrustedDNSServerClosedError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "server shutdown") || strings.Contains(message, "server closed")
}

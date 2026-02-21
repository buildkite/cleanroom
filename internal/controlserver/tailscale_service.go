package controlserver

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"

	"github.com/buildkite/cleanroom/internal/endpoint"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

type tailscaleLocalClient interface {
	StatusWithoutPeers(ctx context.Context) (*ipnstate.Status, error)
	GetServeConfig(ctx context.Context) (*ipn.ServeConfig, error)
	SetServeConfig(ctx context.Context, config *ipn.ServeConfig) error
	GetPrefs(ctx context.Context) (*ipn.Prefs, error)
	EditPrefs(ctx context.Context, prefs *ipn.MaskedPrefs) (*ipn.Prefs, error)
}

var newTailscaleLocalClient = func() tailscaleLocalClient {
	return &tailscale.LocalClient{}
}

func configureTailscaleService(ctx context.Context, ep endpoint.Endpoint, localAddr string) error {
	serviceName := tailcfg.ServiceName(strings.TrimSpace(ep.TSServiceName))
	if err := serviceName.Validate(); err != nil {
		return fmt.Errorf("invalid tailscale service name %q: %w", ep.TSServiceName, err)
	}
	if strings.TrimSpace(localAddr) == "" {
		return fmt.Errorf("empty local listen address for service %q", serviceName)
	}

	lc := newTailscaleLocalClient()
	status, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return fmt.Errorf("get tailscale status: %w", err)
	}
	if status == nil {
		return fmt.Errorf("tailscale status missing tailnet MagicDNS suffix")
	}
	suffix := ""
	if status.CurrentTailnet != nil {
		suffix = strings.TrimSpace(status.CurrentTailnet.MagicDNSSuffix)
	}
	if suffix == "" {
		suffix = strings.TrimSpace(status.MagicDNSSuffix)
	}
	if suffix == "" {
		return fmt.Errorf("tailscale status missing tailnet MagicDNS suffix")
	}
	serviceHost := fmt.Sprintf("%s.%s", serviceName.WithoutPrefix(), suffix)
	proxyTarget := fmt.Sprintf("http://%s", localAddr)

	serveConfig, err := lc.GetServeConfig(ctx)
	if err != nil {
		return fmt.Errorf("get tailscale serve config: %w", err)
	}
	if serveConfig == nil {
		serveConfig = &ipn.ServeConfig{}
	}

	desiredServiceConfig := &ipn.ServiceConfig{
		TCP: map[uint16]*ipn.TCPPortHandler{
			443: {
				HTTPS: true,
			},
		},
		Web: map[ipn.HostPort]*ipn.WebServerConfig{
			ipn.HostPort(net.JoinHostPort(serviceHost, "443")): {
				Handlers: map[string]*ipn.HTTPHandler{
					"/": {
						Proxy: proxyTarget,
					},
				},
			},
		},
	}

	currentServiceConfig := serveConfig.Services[serviceName]
	if !reflect.DeepEqual(currentServiceConfig, desiredServiceConfig) {
		if serveConfig.Services == nil {
			serveConfig.Services = map[tailcfg.ServiceName]*ipn.ServiceConfig{}
		}
		serveConfig.Services[serviceName] = desiredServiceConfig
		if err := lc.SetServeConfig(ctx, serveConfig); err != nil {
			return fmt.Errorf("set tailscale serve config for %q: %w", serviceName, err)
		}
	}

	prefs, err := lc.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("get tailscale prefs: %w", err)
	}
	advertiseServices := []string{}
	if prefs != nil {
		advertiseServices = append(advertiseServices, prefs.AdvertiseServices...)
	}
	if slices.Contains(advertiseServices, serviceName.String()) {
		return nil
	}

	advertiseServices = append(advertiseServices, serviceName.String())
	if _, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		AdvertiseServicesSet: true,
		Prefs: ipn.Prefs{
			AdvertiseServices: advertiseServices,
		},
	}); err != nil {
		return fmt.Errorf("advertise tailscale service %q: %w", serviceName, err)
	}
	return nil
}

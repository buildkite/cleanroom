//go:build libspore

package sporevm

import (
	"context"
	"encoding/json"
	"fmt"

	spore "github.com/sporevm/sporevm/bindings/go"
)

type client struct {
	inner           *spore.Client
	sporeExecutable string
}

func New() (Client, error) {
	inner, err := spore.New()
	if err != nil {
		return nil, err
	}
	for name, value := range contextEnvFromProcess() {
		if err := inner.SetEnv(context.Background(), name, value); err != nil {
			inner.Close()
			return nil, fmt.Errorf("set libspore environment %s: %w", name, err)
		}
	}
	return &client{inner: inner, sporeExecutable: defaultSporeExecutable()}, nil
}

func (c *client) NetworkCapabilities(ctx context.Context) (NetworkCapabilities, error) {
	if err := c.ready(); err != nil {
		return NetworkCapabilities{}, err
	}
	caps, err := c.inner.NetworkCapabilities(ctx)
	if err != nil {
		return NetworkCapabilities{}, err
	}
	return NetworkCapabilities{
		Supported:     caps.Supported,
		ExactHostPort: caps.ExactHostPort,
		BoundServices: caps.BoundServices,
	}, nil
}

func (c *client) Close() error {
	if c == nil || c.inner == nil {
		return nil
	}
	c.inner.Close()
	c.inner = nil
	return nil
}

func (c *client) InspectSpore(ctx context.Context, options InspectSporeOptions) (SporeInspectResult, error) {
	if err := c.ready(); err != nil {
		return SporeInspectResult{}, err
	}
	result, err := c.inner.InspectSpore(ctx, spore.InspectSporeOptions{
		SporeDir: options.SporeDir,
	})
	if err != nil {
		return SporeInspectResult{}, err
	}
	return SporeInspectResult{
		Annotations: result.Annotations,
	}, nil
}

func (c *client) CreateNamed(ctx context.Context, options CreateNamedOptions) (JSONResult, error) {
	if err := c.ready(); err != nil {
		return JSONResult{}, err
	}
	return jsonResult(c.inner.CreateNamed(ctx, spore.CreateNamedOptions{
		Name:            options.Name,
		Backend:         options.Backend,
		SporeExecutable: c.sporeExecutable,
		ImageRef:        options.ImageRef,
		MemoryBytes:     options.MemoryBytes,
		VCPUs:           options.VCPUs,
		TimeoutMs:       options.TimeoutMS,
		NetworkEnabled:  options.NetworkEnabled,
		NetworkRules:    sporeNetworkRules(options.NetworkRules),
		BoundServices:   sporeBoundUnixServices(options.BoundServices),
		Annotations:     options.Annotations,
	}))
}

func (c *client) ExecNamed(ctx context.Context, options ExecNamedOptions) (JSONResult, error) {
	if err := c.ready(); err != nil {
		return JSONResult{}, err
	}
	return jsonResult(c.inner.ExecNamed(ctx, spore.ExecNamedOptions{
		Name: options.Name,
		Argv: options.Argv,
	}))
}

func (c *client) ResumeNamed(ctx context.Context, options ResumeNamedOptions) (JSONResult, error) {
	if err := c.ready(); err != nil {
		return JSONResult{}, err
	}
	return jsonResult(c.inner.ResumeNamed(ctx, spore.ResumeNamedOptions{
		SporeDir:             options.SporeDir,
		Name:                 options.Name,
		SporeExecutable:      c.sporeExecutable,
		BoundServiceBindings: sporeBoundUnixServiceBindings(options.BoundServiceBindings),
	}))
}

func (c *client) SnapshotNamed(ctx context.Context, options SnapshotNamedOptions) (JSONResult, error) {
	if err := c.ready(); err != nil {
		return JSONResult{}, err
	}
	return jsonResult(c.inner.SnapshotNamed(ctx, spore.SnapshotNamedOptions{
		Name:        options.Name,
		OutDir:      options.OutDir,
		Continue:    options.Continue,
		Annotations: options.Annotations,
	}))
}

func (c *client) RemoveNamed(ctx context.Context, options RemoveNamedOptions) (JSONResult, error) {
	if err := c.ready(); err != nil {
		return JSONResult{}, err
	}
	return jsonResult(c.inner.RemoveNamed(ctx, spore.RemoveNamedOptions{Name: options.Name}))
}

func (c *client) ready() error {
	if c == nil || c.inner == nil {
		return ErrClosed
	}
	return nil
}

func jsonResult[T any](value T, err error) (JSONResult, error) {
	if err != nil {
		return JSONResult{}, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return JSONResult{}, fmt.Errorf("encode libspore result: %w", err)
	}
	return JSONResult{RawJSON: raw}, nil
}

func sporeNetworkRules(rules []NetworkRule) []spore.NetworkRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]spore.NetworkRule, 0, len(rules))
	for _, rule := range rules {
		out = append(out, spore.NetworkRule{
			Host:  rule.Host,
			Ports: append([]uint16(nil), rule.Ports...),
		})
	}
	return out
}

func sporeBoundUnixServices(services []BoundUnixService) []spore.BoundUnixService {
	if len(services) == 0 {
		return nil
	}
	out := make([]spore.BoundUnixService, 0, len(services))
	for _, service := range services {
		out = append(out, spore.BoundUnixService{
			Name:      service.Name,
			GuestHost: service.GuestHost,
			GuestPort: service.GuestPort,
			UnixPath:  service.UnixPath,
		})
	}
	return out
}

func sporeBoundUnixServiceBindings(bindings []BoundUnixServiceBinding) []spore.BoundUnixServiceBinding {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]spore.BoundUnixServiceBinding, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, spore.BoundUnixServiceBinding{
			Name:     binding.Name,
			UnixPath: binding.UnixPath,
		})
	}
	return out
}

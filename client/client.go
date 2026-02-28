package client

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
)

// Client is the public Go client for the cleanroom control-plane API.
type Client struct {
	inner *controlclient.Client

	mu           sync.Mutex
	sandboxByKey map[string]string
	ensureLocks  map[string]*ensureKeyLock
}

type ensureKeyLock struct {
	mu   sync.Mutex
	refs int
}

// TLSOptions configures optional TLS material for HTTPS connections.
type TLSOptions struct {
	CertPath string
	KeyPath  string
	CAPath   string
}

// Option configures the cleanroom client.
type Option func(*options)

type options struct {
	tls tlsconfig.Options
}

// WithTLS configures TLS options for HTTPS endpoints.
func WithTLS(opts TLSOptions) Option {
	return func(o *options) {
		o.tls = tlsconfig.Options{
			CertPath: opts.CertPath,
			KeyPath:  opts.KeyPath,
			CAPath:   opts.CAPath,
		}
	}
}

// New creates a client for the provided endpoint.
//
// Supported endpoint formats match the CLI:
// - unix:///path/to/cleanroom.sock
// - absolute unix socket path
// - http://host:port
// - https://host:port
//
// If host is empty, CLEANROOM_HOST is used, then the default unix socket path.
func New(host string, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	ep, err := endpoint.Resolve(host)
	if err != nil {
		return nil, err
	}
	if ep.Scheme == "tssvc" {
		return nil, errors.New("tssvc:// endpoints are listen-only; use https://<service>.<your-tailnet>.ts.net")
	}
	inner, err := controlclient.New(ep, controlclient.WithTLS(o.tls))
	if err != nil {
		return nil, err
	}
	return &Client{
		inner:        inner,
		sandboxByKey: map[string]string{},
		ensureLocks:  map[string]*ensureKeyLock{},
	}, nil
}

func (c *Client) CreateSandbox(ctx context.Context, req *CreateSandboxRequest) (*CreateSandboxResponse, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.CreateSandbox(ctx, req)
}

func (c *Client) GetSandbox(ctx context.Context, req *GetSandboxRequest) (*GetSandboxResponse, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.GetSandbox(ctx, req)
}

func (c *Client) ListSandboxes(ctx context.Context, req *ListSandboxesRequest) (*ListSandboxesResponse, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.ListSandboxes(ctx, req)
}

func (c *Client) DownloadSandboxFile(ctx context.Context, req *DownloadSandboxFileRequest) (*DownloadSandboxFileResponse, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.DownloadSandboxFile(ctx, req)
}

func (c *Client) TerminateSandbox(ctx context.Context, req *TerminateSandboxRequest) (*TerminateSandboxResponse, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.TerminateSandbox(ctx, req)
}

func (c *Client) StreamSandboxEvents(ctx context.Context, req *StreamSandboxEventsRequest) (*connect.ServerStreamForClient[SandboxEvent], error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.StreamSandboxEvents(ctx, req)
}

func (c *Client) CreateExecution(ctx context.Context, req *CreateExecutionRequest) (*CreateExecutionResponse, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.CreateExecution(ctx, req)
}

func (c *Client) GetExecution(ctx context.Context, req *GetExecutionRequest) (*GetExecutionResponse, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.GetExecution(ctx, req)
}

func (c *Client) CancelExecution(ctx context.Context, req *CancelExecutionRequest) (*CancelExecutionResponse, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.CancelExecution(ctx, req)
}

func (c *Client) StreamExecution(ctx context.Context, req *StreamExecutionRequest) (*connect.ServerStreamForClient[ExecutionStreamEvent], error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("nil client")
	}
	return c.inner.StreamExecution(ctx, req)
}

func (c *Client) AttachExecution(ctx context.Context) *connect.BidiStreamForClient[ExecutionAttachFrame, ExecutionAttachFrame] {
	if c == nil || c.inner == nil {
		return nil
	}
	return c.inner.AttachExecution(ctx)
}

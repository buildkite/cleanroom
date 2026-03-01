package controlclient

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
	"golang.org/x/net/http2"
)

type Client struct {
	httpClient      *http.Client
	baseURL         string
	sandboxClient   cleanroomv1connect.SandboxServiceClient
	executionClient cleanroomv1connect.ExecutionServiceClient
}

// Option configures the client.
type Option func(*options)

type options struct {
	tlsOpts tlsconfig.Options
}

// WithTLS configures TLS options for the client.
func WithTLS(opts tlsconfig.Options) Option {
	return func(o *options) {
		o.tlsOpts = opts
	}
}

func New(ep endpoint.Endpoint, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	baseURL := strings.TrimRight(ep.BaseURL, "/")
	transport, err := buildTransport(ep, baseURL, o.tlsOpts)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Transport: transport}
	return &Client{
		httpClient:      httpClient,
		baseURL:         baseURL,
		sandboxClient:   cleanroomv1connect.NewSandboxServiceClient(httpClient, baseURL),
		executionClient: cleanroomv1connect.NewExecutionServiceClient(httpClient, baseURL),
	}, nil
}

func buildTransport(ep endpoint.Endpoint, baseURL string, tlsOpts tlsconfig.Options) (http.RoundTripper, error) {
	dialer := &net.Dialer{}

	if ep.Scheme == "https" {
		tlsCfg, err := tlsconfig.ResolveClient(tlsOpts)
		if err != nil {
			return nil, err
		}
		if tlsCfg == nil {
			tlsCfg = &tls.Config{MinVersion: tls.VersionTLS13}
		}
		return &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			TLSClientConfig:   tlsCfg,
			ForceAttemptHTTP2: true,
		}, nil
	}

	if ep.Scheme == "unix" {
		return &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", ep.Address)
			},
		}, nil
	}

	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return &http.Transport{}, nil
	}
	host := parsed.Host
	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", host)
		},
	}, nil
}

func (c *Client) CreateSandbox(ctx context.Context, req *cleanroomv1.CreateSandboxRequest) (*cleanroomv1.CreateSandboxResponse, error) {
	resp, err := c.sandboxClient.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) GetSandbox(ctx context.Context, req *cleanroomv1.GetSandboxRequest) (*cleanroomv1.GetSandboxResponse, error) {
	resp, err := c.sandboxClient.GetSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) ListSandboxes(ctx context.Context, req *cleanroomv1.ListSandboxesRequest) (*cleanroomv1.ListSandboxesResponse, error) {
	resp, err := c.sandboxClient.ListSandboxes(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) DownloadSandboxFile(ctx context.Context, req *cleanroomv1.DownloadSandboxFileRequest) (*cleanroomv1.DownloadSandboxFileResponse, error) {
	resp, err := c.sandboxClient.DownloadSandboxFile(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) TerminateSandbox(ctx context.Context, req *cleanroomv1.TerminateSandboxRequest) (*cleanroomv1.TerminateSandboxResponse, error) {
	resp, err := c.sandboxClient.TerminateSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) StreamSandboxEvents(ctx context.Context, req *cleanroomv1.StreamSandboxEventsRequest) (*connect.ServerStreamForClient[cleanroomv1.SandboxEvent], error) {
	return c.sandboxClient.StreamSandboxEvents(ctx, connect.NewRequest(req))
}

func (c *Client) CreateExecution(ctx context.Context, req *cleanroomv1.CreateExecutionRequest) (*cleanroomv1.CreateExecutionResponse, error) {
	resp, err := c.executionClient.CreateExecution(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) OpenInteractiveExecution(ctx context.Context, req *cleanroomv1.OpenInteractiveExecutionRequest) (*cleanroomv1.OpenInteractiveExecutionResponse, error) {
	resp, err := c.executionClient.OpenInteractiveExecution(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) GetExecution(ctx context.Context, req *cleanroomv1.GetExecutionRequest) (*cleanroomv1.GetExecutionResponse, error) {
	resp, err := c.executionClient.GetExecution(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) CancelExecution(ctx context.Context, req *cleanroomv1.CancelExecutionRequest) (*cleanroomv1.CancelExecutionResponse, error) {
	resp, err := c.executionClient.CancelExecution(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *Client) StreamExecution(ctx context.Context, req *cleanroomv1.StreamExecutionRequest) (*connect.ServerStreamForClient[cleanroomv1.ExecutionStreamEvent], error) {
	return c.executionClient.StreamExecution(ctx, connect.NewRequest(req))
}

func (c *Client) AttachExecution(ctx context.Context) *connect.BidiStreamForClient[cleanroomv1.ExecutionAttachFrame, cleanroomv1.ExecutionAttachFrame] {
	return c.executionClient.AttachExecution(ctx)
}

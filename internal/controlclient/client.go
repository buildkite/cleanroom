package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/controlapi"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
)

type Client struct {
	httpClient      *http.Client
	baseURL         string
	sandboxClient   cleanroomv1connect.SandboxServiceClient
	executionClient cleanroomv1connect.ExecutionServiceClient
}

func New(ep endpoint.Endpoint) *Client {
	transport := &http.Transport{}
	if ep.Scheme == "unix" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", ep.Address)
		}
	}

	httpClient := &http.Client{Transport: transport}
	baseURL := strings.TrimRight(ep.BaseURL, "/")
	return &Client{
		httpClient:      httpClient,
		baseURL:         baseURL,
		sandboxClient:   cleanroomv1connect.NewSandboxServiceClient(httpClient, baseURL),
		executionClient: cleanroomv1connect.NewExecutionServiceClient(httpClient, baseURL),
	}
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

// Backward-compatible JSON client methods.
func (c *Client) Exec(ctx context.Context, req controlapi.ExecRequest) (*controlapi.ExecResponse, error) {
	out := controlapi.ExecResponse{}
	if err := c.postJSON(ctx, "/v1/exec", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) LaunchCleanroom(ctx context.Context, req controlapi.LaunchCleanroomRequest) (*controlapi.LaunchCleanroomResponse, error) {
	out := controlapi.LaunchCleanroomResponse{}
	if err := c.postJSON(ctx, "/v1/cleanrooms/launch", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RunCleanroom(ctx context.Context, req controlapi.RunCleanroomRequest) (*controlapi.RunCleanroomResponse, error) {
	out := controlapi.RunCleanroomResponse{}
	if err := c.postJSON(ctx, "/v1/cleanrooms/run", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) TerminateCleanroom(ctx context.Context, req controlapi.TerminateCleanroomRequest) (*controlapi.TerminateCleanroomResponse, error) {
	out := controlapi.TerminateCleanroomResponse{}
	if err := c.postJSON(ctx, "/v1/cleanrooms/terminate", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) postJSON(ctx context.Context, path string, in any, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		errBody := controlapi.ErrorResponse{}
		if decodeErr := json.NewDecoder(httpResp.Body).Decode(&errBody); decodeErr != nil {
			return fmt.Errorf("request to %s failed with status %d", path, httpResp.StatusCode)
		}
		if errBody.Error == "" {
			return fmt.Errorf("request to %s failed with status %d", path, httpResp.StatusCode)
		}
		return errors.New(errBody.Error)
	}

	return json.NewDecoder(httpResp.Body).Decode(out)
}

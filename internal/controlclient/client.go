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

	"github.com/buildkite/cleanroom/internal/controlapi"
	"github.com/buildkite/cleanroom/internal/endpoint"
)

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func New(ep endpoint.Endpoint) *Client {
	transport := &http.Transport{}
	if ep.Scheme == "unix" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", ep.Address)
		}
	}

	baseURL := strings.TrimRight(ep.BaseURL, "/")
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
		},
		baseURL: baseURL,
	}
}

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

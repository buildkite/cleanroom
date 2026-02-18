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
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/exec", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		errBody := controlapi.ErrorResponse{}
		if decodeErr := json.NewDecoder(httpResp.Body).Decode(&errBody); decodeErr != nil {
			return nil, fmt.Errorf("exec request failed with status %d", httpResp.StatusCode)
		}
		if errBody.Error == "" {
			return nil, fmt.Errorf("exec request failed with status %d", httpResp.StatusCode)
		}
		return nil, errors.New(errBody.Error)
	}

	out := controlapi.ExecResponse{}
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

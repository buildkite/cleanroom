package vsockexec

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const DefaultPort uint32 = 10700

type ExecRequest struct {
	Command         []string `json:"command"`
	Dir             string   `json:"dir,omitempty"`
	Env             []string `json:"env,omitempty"`
	WorkspaceTarGz  []byte   `json:"workspace_tar_gz,omitempty"`
	WorkspaceAccess string   `json:"workspace_access,omitempty"` // rw|ro
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

func DecodeRequest(r io.Reader) (ExecRequest, error) {
	var req ExecRequest
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return ExecRequest{}, err
	}
	if len(req.Command) == 0 {
		return ExecRequest{}, errors.New("missing command")
	}
	for i := range req.Command {
		req.Command[i] = strings.TrimSpace(req.Command[i])
	}
	if req.Command[0] == "" {
		return ExecRequest{}, errors.New("missing command executable")
	}
	return req, nil
}

func EncodeRequest(w io.Writer, req ExecRequest) error {
	return json.NewEncoder(w).Encode(req)
}

func DecodeResponse(r io.Reader) (ExecResponse, error) {
	var res ExecResponse
	if err := json.NewDecoder(r).Decode(&res); err != nil {
		return ExecResponse{}, err
	}
	return res, nil
}

func EncodeResponse(w io.Writer, res ExecResponse) error {
	return json.NewEncoder(w).Encode(res)
}

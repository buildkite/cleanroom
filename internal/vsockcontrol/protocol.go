package vsockcontrol

import (
	"encoding/json"
	"io"
)

const DefaultHostControlPort uint32 = 10701

// CheckpointRequest is sent from guest to host to request a snapshot
// at a safe point.
type CheckpointRequest struct {
	Name string `json:"name,omitempty"`
}

// CheckpointResponse is sent from host to guest in reply.
type CheckpointResponse struct {
	Accepted bool   `json:"accepted"`
	Pending  bool   `json:"pending,omitempty"`
	Message  string `json:"message,omitempty"`
	Error    string `json:"error,omitempty"`
}

// QuiesceRequest is sent from host to guest on the existing guest-agent
// vsock port. The guest should sync filesystems and run any registered
// quiesce hook before responding.
type QuiesceRequest struct {
	Type           string `json:"type"`                      // "quiesce"
	TimeoutSeconds int64  `json:"timeout_seconds,omitempty"`
}

// QuiesceResponse is sent from guest to host after quiesce completes.
type QuiesceResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func EncodeCheckpointRequest(w io.Writer, req CheckpointRequest) error {
	return json.NewEncoder(w).Encode(req)
}

func DecodeCheckpointRequest(r io.Reader) (CheckpointRequest, error) {
	var req CheckpointRequest
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return CheckpointRequest{}, err
	}
	return req, nil
}

func EncodeCheckpointResponse(w io.Writer, resp CheckpointResponse) error {
	return json.NewEncoder(w).Encode(resp)
}

func DecodeCheckpointResponse(r io.Reader) (CheckpointResponse, error) {
	var resp CheckpointResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return CheckpointResponse{}, err
	}
	return resp, nil
}

func EncodeQuiesceRequest(w io.Writer, req QuiesceRequest) error {
	return json.NewEncoder(w).Encode(req)
}

func DecodeQuiesceRequest(r io.Reader) (QuiesceRequest, error) {
	var req QuiesceRequest
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return QuiesceRequest{}, err
	}
	return req, nil
}

func EncodeQuiesceResponse(w io.Writer, resp QuiesceResponse) error {
	return json.NewEncoder(w).Encode(resp)
}

func DecodeQuiesceResponse(r io.Reader) (QuiesceResponse, error) {
	var resp QuiesceResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return QuiesceResponse{}, err
	}
	return resp, nil
}

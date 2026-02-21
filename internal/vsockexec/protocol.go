package vsockexec

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const DefaultPort uint32 = 10700

type ExecRequest struct {
	Command         []string `json:"command"`
	Dir         string   `json:"dir,omitempty"`
	Env         []string `json:"env,omitempty"`
	EntropySeed []byte   `json:"entropy_seed,omitempty"`
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

type ExecStreamFrame struct {
	Type     string `json:"type,omitempty"` // stdout|stderr|exit
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
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

func EncodeStreamFrame(w io.Writer, frame ExecStreamFrame) error {
	return json.NewEncoder(w).Encode(frame)
}

type StreamCallbacks struct {
	OnStdout func([]byte)
	OnStderr func([]byte)
}

func DecodeStreamResponse(r io.Reader, callbacks StreamCallbacks) (ExecResponse, error) {
	dec := json.NewDecoder(r)
	out := ExecResponse{}
	for {
		raw := map[string]json.RawMessage{}
		if err := dec.Decode(&raw); err != nil {
			return ExecResponse{}, err
		}

		typeRaw, hasType := raw["type"]
		if !hasType {
			// Backward compatibility with single ExecResponse payload.
			payload, err := json.Marshal(raw)
			if err != nil {
				return ExecResponse{}, err
			}
			res := ExecResponse{}
			if err := json.Unmarshal(payload, &res); err != nil {
				return ExecResponse{}, err
			}
			return res, nil
		}

		kind := ""
		if err := json.Unmarshal(typeRaw, &kind); err != nil {
			return ExecResponse{}, err
		}
		kind = strings.ToLower(strings.TrimSpace(kind))

		switch kind {
		case "stdout", "stderr":
			chunk, err := decodeFrameData(raw["data"])
			if err != nil {
				return ExecResponse{}, err
			}
			if len(chunk) == 0 {
				continue
			}
			if kind == "stdout" {
				out.Stdout += string(chunk)
				if callbacks.OnStdout != nil {
					callbacks.OnStdout(append([]byte(nil), chunk...))
				}
			} else {
				out.Stderr += string(chunk)
				if callbacks.OnStderr != nil {
					callbacks.OnStderr(append([]byte(nil), chunk...))
				}
			}
		case "exit":
			if exitRaw, ok := raw["exit_code"]; ok {
				if err := json.Unmarshal(exitRaw, &out.ExitCode); err != nil {
					return ExecResponse{}, err
				}
			}
			if errRaw, ok := raw["error"]; ok {
				if err := json.Unmarshal(errRaw, &out.Error); err != nil {
					return ExecResponse{}, err
				}
			}
			return out, nil
		default:
			return ExecResponse{}, fmt.Errorf("unknown stream frame type %q", kind)
		}
	}
}

func decodeFrameData(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var b []byte
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}
	// Be tolerant of non-base64 string values.
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err == nil {
		return decoded, nil
	}
	return []byte(s), nil
}

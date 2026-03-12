package vsockcontrol

import (
	"bytes"
	"testing"
)

func TestCheckpointRequestRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	req := CheckpointRequest{Name: "before-deploy"}
	if err := EncodeCheckpointRequest(&buf, req); err != nil {
		t.Fatalf("EncodeCheckpointRequest: %v", err)
	}
	got, err := DecodeCheckpointRequest(&buf)
	if err != nil {
		t.Fatalf("DecodeCheckpointRequest: %v", err)
	}
	if got.Name != req.Name {
		t.Fatalf("Name: got %q, want %q", got.Name, req.Name)
	}
}

func TestCheckpointResponseRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	resp := CheckpointResponse{Accepted: true, Pending: true, Message: "snapshot queued"}
	if err := EncodeCheckpointResponse(&buf, resp); err != nil {
		t.Fatalf("EncodeCheckpointResponse: %v", err)
	}
	got, err := DecodeCheckpointResponse(&buf)
	if err != nil {
		t.Fatalf("DecodeCheckpointResponse: %v", err)
	}
	if got.Accepted != resp.Accepted {
		t.Fatalf("Accepted: got %v, want %v", got.Accepted, resp.Accepted)
	}
	if got.Pending != resp.Pending {
		t.Fatalf("Pending: got %v, want %v", got.Pending, resp.Pending)
	}
	if got.Message != resp.Message {
		t.Fatalf("Message: got %q, want %q", got.Message, resp.Message)
	}
}

func TestCheckpointResponseError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	resp := CheckpointResponse{Accepted: false, Error: "quota exceeded"}
	if err := EncodeCheckpointResponse(&buf, resp); err != nil {
		t.Fatalf("EncodeCheckpointResponse: %v", err)
	}
	got, err := DecodeCheckpointResponse(&buf)
	if err != nil {
		t.Fatalf("DecodeCheckpointResponse: %v", err)
	}
	if got.Accepted {
		t.Fatal("expected Accepted=false")
	}
	if got.Error != resp.Error {
		t.Fatalf("Error: got %q, want %q", got.Error, resp.Error)
	}
}

func TestQuiesceRequestRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	req := QuiesceRequest{Type: "quiesce", TimeoutSeconds: 30}
	if err := EncodeQuiesceRequest(&buf, req); err != nil {
		t.Fatalf("EncodeQuiesceRequest: %v", err)
	}
	got, err := DecodeQuiesceRequest(&buf)
	if err != nil {
		t.Fatalf("DecodeQuiesceRequest: %v", err)
	}
	if got.Type != req.Type {
		t.Fatalf("Type: got %q, want %q", got.Type, req.Type)
	}
	if got.TimeoutSeconds != req.TimeoutSeconds {
		t.Fatalf("TimeoutSeconds: got %d, want %d", got.TimeoutSeconds, req.TimeoutSeconds)
	}
}

func TestQuiesceResponseRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	resp := QuiesceResponse{OK: true}
	if err := EncodeQuiesceResponse(&buf, resp); err != nil {
		t.Fatalf("EncodeQuiesceResponse: %v", err)
	}
	got, err := DecodeQuiesceResponse(&buf)
	if err != nil {
		t.Fatalf("DecodeQuiesceResponse: %v", err)
	}
	if !got.OK {
		t.Fatal("expected OK=true")
	}
	if got.Error != "" {
		t.Fatalf("unexpected Error: %q", got.Error)
	}
}

func TestQuiesceResponseError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	resp := QuiesceResponse{OK: false, Error: "hook failed"}
	if err := EncodeQuiesceResponse(&buf, resp); err != nil {
		t.Fatalf("EncodeQuiesceResponse: %v", err)
	}
	got, err := DecodeQuiesceResponse(&buf)
	if err != nil {
		t.Fatalf("DecodeQuiesceResponse: %v", err)
	}
	if got.OK {
		t.Fatal("expected OK=false")
	}
	if got.Error != resp.Error {
		t.Fatalf("Error: got %q, want %q", got.Error, resp.Error)
	}
}

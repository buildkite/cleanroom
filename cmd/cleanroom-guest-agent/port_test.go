package main

import (
	"strings"
	"testing"
)

func TestResolveGuestAgentPortPrefersEnvironment(t *testing.T) {
	t.Parallel()

	port, err := resolveGuestAgentPort(10700, "12345", "console=hvc0 cleanroom_guest_port=22222")
	if err != nil {
		t.Fatalf("resolveGuestAgentPort returned error: %v", err)
	}
	if got, want := port, uint32(12345); got != want {
		t.Fatalf("unexpected port: got %d want %d", got, want)
	}
}

func TestResolveGuestAgentPortFallsBackToKernelCmdline(t *testing.T) {
	t.Parallel()

	port, err := resolveGuestAgentPort(10700, "", "console=hvc0 cleanroom_guest_port=22222")
	if err != nil {
		t.Fatalf("resolveGuestAgentPort returned error: %v", err)
	}
	if got, want := port, uint32(22222); got != want {
		t.Fatalf("unexpected port: got %d want %d", got, want)
	}
}

func TestResolveGuestAgentPortUsesDefaultWhenUnset(t *testing.T) {
	t.Parallel()

	port, err := resolveGuestAgentPort(10700, "", "console=hvc0 root=/dev/vda")
	if err != nil {
		t.Fatalf("resolveGuestAgentPort returned error: %v", err)
	}
	if got, want := port, uint32(10700); got != want {
		t.Fatalf("unexpected port: got %d want %d", got, want)
	}
}

func TestResolveGuestAgentPortRejectsInvalidEnvironmentValue(t *testing.T) {
	t.Parallel()

	_, err := resolveGuestAgentPort(10700, "not-a-port", "cleanroom_guest_port=22222")
	if err == nil {
		t.Fatal("expected error for invalid environment port")
	}
	if !strings.Contains(err.Error(), "CLEANROOM_VSOCK_PORT") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveGuestAgentPortRejectsInvalidKernelCmdlineValue(t *testing.T) {
	t.Parallel()

	_, err := resolveGuestAgentPort(10700, "", "cleanroom_guest_port=not-a-port")
	if err == nil {
		t.Fatal("expected error for invalid kernel cmdline port")
	}
	if !strings.Contains(err.Error(), "cleanroom_guest_port") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestKernelCmdlineValueReturnsExactKeyMatch(t *testing.T) {
	t.Parallel()

	if value, ok := kernelCmdlineValue("xcleanroom_guest_port=9999 cleanroom_guest_port=22222", "cleanroom_guest_port"); !ok || value != "22222" {
		t.Fatalf("unexpected key parse result: ok=%t value=%q", ok, value)
	}
}

package main

import (
	"errors"
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

func TestReadKernelCmdlineWithRetriesAfterMountingProc(t *testing.T) {
	t.Parallel()

	readCalls := 0
	mountCalls := 0
	got := readKernelCmdlineWith(
		func(path string) ([]byte, error) {
			if path != "/proc/cmdline" {
				t.Fatalf("unexpected read path: %q", path)
			}
			readCalls++
			if readCalls == 1 {
				return nil, errors.New("not mounted")
			}
			return []byte("console=hvc0 cleanroom_guest_port=22222"), nil
		},
		func() error {
			mountCalls++
			return nil
		},
	)

	if got != "console=hvc0 cleanroom_guest_port=22222" {
		t.Fatalf("unexpected cmdline: %q", got)
	}
	if readCalls != 2 {
		t.Fatalf("expected two read attempts, got %d", readCalls)
	}
	if mountCalls != 1 {
		t.Fatalf("expected one mount attempt, got %d", mountCalls)
	}
}

func TestReadKernelCmdlineWithSkipsMountWhenReadSucceeds(t *testing.T) {
	t.Parallel()

	mountCalls := 0
	got := readKernelCmdlineWith(
		func(path string) ([]byte, error) {
			if path != "/proc/cmdline" {
				t.Fatalf("unexpected read path: %q", path)
			}
			return []byte("console=hvc0"), nil
		},
		func() error {
			mountCalls++
			return nil
		},
	)

	if got != "console=hvc0" {
		t.Fatalf("unexpected cmdline: %q", got)
	}
	if mountCalls != 0 {
		t.Fatalf("expected no mount attempts, got %d", mountCalls)
	}
}

func TestReadKernelCmdlineWithReturnsEmptyWhenMountFails(t *testing.T) {
	t.Parallel()

	got := readKernelCmdlineWith(
		func(path string) ([]byte, error) {
			if path != "/proc/cmdline" {
				t.Fatalf("unexpected read path: %q", path)
			}
			return nil, errors.New("not mounted")
		},
		func() error {
			return errors.New("mount failed")
		},
	)

	if got != "" {
		t.Fatalf("expected empty cmdline, got %q", got)
	}
}

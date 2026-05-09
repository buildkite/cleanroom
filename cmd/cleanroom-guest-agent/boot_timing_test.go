//go:build linux

package main

import (
	"errors"
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestNewGuestBootTimingStoreParsesEnvAndRecordsAgentStart(t *testing.T) {
	t.Parallel()

	store := newGuestBootTimingStore(
		"guest_init_vsock_agent_exec=0.042,missing-value,bad=abc,negative=-1",
		func() (int64, error) { return 57, nil },
	)

	got := store.snapshot()
	if got[vsockexec.GuestBootTimingInitVSOCKAgentExec] != 42 {
		t.Fatalf("expected env timing to be parsed, got %#v", got)
	}
	if got[vsockexec.GuestBootTimingAgentStart] != 57 {
		t.Fatalf("expected agent start timing to be recorded, got %#v", got)
	}
	if _, ok := got["bad"]; ok {
		t.Fatalf("did not expect invalid timing to be recorded, got %#v", got)
	}
}

func TestNewGuestBootTimingStoreFromEnvDisablesTimingWhenUnset(t *testing.T) {
	t.Parallel()

	store := newGuestBootTimingStoreFromEnv("")
	store.record(vsockexec.GuestBootTimingAgentFirstAccept)

	if got := store.snapshot(); got != nil {
		t.Fatalf("expected disabled timing snapshot to be nil, got %#v", got)
	}
}

func TestGuestBootTimingStoreRecordOnceKeepsFirstValue(t *testing.T) {
	t.Parallel()

	values := []int64{10, 20, 30}
	store := newGuestBootTimingStore("", func() (int64, error) {
		next := values[0]
		values = values[1:]
		return next, nil
	})

	store.recordOnce(vsockexec.GuestBootTimingAgentFirstAccept)
	store.recordOnce(vsockexec.GuestBootTimingAgentFirstAccept)
	store.record(vsockexec.GuestBootTimingAgentListenReady)

	got := store.snapshot()
	if got[vsockexec.GuestBootTimingAgentFirstAccept] != 20 {
		t.Fatalf("expected first accept to keep first value, got %#v", got)
	}
	if got[vsockexec.GuestBootTimingAgentListenReady] != 30 {
		t.Fatalf("expected explicit record to update value, got %#v", got)
	}
}

func TestGuestBootTimingStoreSkipsClockErrors(t *testing.T) {
	t.Parallel()

	store := newGuestBootTimingStore("", func() (int64, error) {
		return 0, errors.New("no clock")
	})

	if got := store.snapshot(); len(got) != 0 {
		t.Fatalf("expected empty timing snapshot, got %#v", got)
	}
}

func TestParseProcUptimeMS(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want int64
	}{
		{name: "fraction", raw: "12.34 56.78\n", want: 12340},
		{name: "millisecond precision", raw: "12.345 56.78\n", want: 12345},
		{name: "whole seconds", raw: "12 56.78\n", want: 12000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProcUptimeMS(tc.raw)
			if err != nil {
				t.Fatalf("parseProcUptimeMS returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseProcUptimeMS: got %d want %d", got, tc.want)
			}
		})
	}
}

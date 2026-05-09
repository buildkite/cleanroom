//go:build darwin

package darwinvz

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestDarwinVZGuestBootPhaseTimings(t *testing.T) {
	t.Parallel()

	got := darwinVZGuestBootPhaseTimings(map[string]int64{
		vsockexec.GuestBootTimingInitVSOCKAgentExec:      190,
		vsockexec.GuestBootTimingAgentStart:              225,
		vsockexec.GuestBootTimingAgentListenReady:        250,
		vsockexec.GuestBootTimingAgentFirstAccept:        280,
		vsockexec.GuestBootTimingAgentFirstRequestDecode: 285,
	})

	for phase, want := range map[string]int64{
		darwinVZTimingGuestInitAgentExec:      190,
		darwinVZTimingGuestAgentStartup:       35,
		darwinVZTimingGuestAgentListen:        25,
		darwinVZTimingGuestAgentAccept:        30,
		darwinVZTimingGuestAgentRequestDecode: 5,
	} {
		if got[phase] != want {
			t.Fatalf("unexpected %s timing: got %d want %d in %#v", phase, got[phase], want, got)
		}
	}
}

func TestDarwinVZGuestBootPhaseTimingsOmitsMissingOrNonPositiveDeltas(t *testing.T) {
	t.Parallel()

	got := darwinVZGuestBootPhaseTimings(map[string]int64{
		vsockexec.GuestBootTimingInitVSOCKAgentExec:      310,
		vsockexec.GuestBootTimingAgentStart:              300,
		vsockexec.GuestBootTimingAgentFirstRequestDecode: 320,
	})

	if got[darwinVZTimingGuestInitAgentExec] != 310 {
		t.Fatalf("unexpected init agent exec timing: got %#v", got)
	}
	if _, ok := got[darwinVZTimingGuestAgentStartup]; ok {
		t.Fatalf("expected non-positive startup delta to be omitted, got %#v", got)
	}
	if _, ok := got[darwinVZTimingGuestAgentRequestDecode]; ok {
		t.Fatalf("expected missing first accept delta to be omitted, got %#v", got)
	}
}

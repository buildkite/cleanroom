//go:build darwin

package darwinvz

import "github.com/buildkite/cleanroom/internal/vsockexec"

const (
	darwinVZTimingGuestInitAgentExec      = "guest_init_agent_exec"
	darwinVZTimingGuestAgentStartup       = "guest_agent_startup"
	darwinVZTimingGuestAgentListen        = "guest_agent_listen"
	darwinVZTimingGuestAgentAccept        = "guest_agent_accept"
	darwinVZTimingGuestAgentRequestDecode = "guest_agent_request_decode"
)

func darwinVZGuestBootPhaseTimings(guestTimingMS map[string]int64) map[string]int64 {
	if len(guestTimingMS) == 0 {
		return nil
	}

	out := map[string]int64{}
	recordGuestTimingFromBoot(out, darwinVZTimingGuestInitAgentExec, guestTimingMS, vsockexec.GuestBootTimingInitVSOCKAgentExec)
	recordGuestTimingDelta(out, darwinVZTimingGuestAgentStartup, guestTimingMS, vsockexec.GuestBootTimingInitVSOCKAgentExec, vsockexec.GuestBootTimingAgentStart)
	recordGuestTimingDelta(out, darwinVZTimingGuestAgentListen, guestTimingMS, vsockexec.GuestBootTimingAgentStart, vsockexec.GuestBootTimingAgentListenReady)
	recordGuestTimingDelta(out, darwinVZTimingGuestAgentAccept, guestTimingMS, vsockexec.GuestBootTimingAgentListenReady, vsockexec.GuestBootTimingAgentFirstAccept)
	recordGuestTimingDelta(out, darwinVZTimingGuestAgentRequestDecode, guestTimingMS, vsockexec.GuestBootTimingAgentFirstAccept, vsockexec.GuestBootTimingAgentFirstRequestDecode)

	if len(out) == 0 {
		return nil
	}
	return out
}

func recordGuestTimingFromBoot(out map[string]int64, phase string, timingMS map[string]int64, endKey string) {
	endMS, ok := timingMS[endKey]
	if !ok || endMS <= 0 {
		return
	}
	out[phase] = endMS
}

func recordGuestTimingDelta(out map[string]int64, phase string, timingMS map[string]int64, startKey, endKey string) {
	startMS, ok := timingMS[startKey]
	if !ok {
		return
	}
	endMS, ok := timingMS[endKey]
	if !ok {
		return
	}
	durationMS := endMS - startMS
	if durationMS <= 0 {
		return
	}
	out[phase] = durationMS
}

//go:build darwin

package darwinvz

import (
	"strings"
	"testing"
)

func TestGuestInitScriptAlwaysStartsStdioAgentWhenSerialDeviceExists(t *testing.T) {
	if strings.Contains(guestInitScriptTemplate, "CLEANROOM_USE_STDIO") {
		t.Fatal("expected stdio transport to no longer be gated by CLEANROOM_USE_STDIO")
	}

	if !strings.Contains(guestInitScriptTemplate, "CLEANROOM_GUEST_TRANSPORT=stdio /usr/local/bin/cleanroom-guest-agent") {
		t.Fatal("expected stdio guest-agent launch in init script")
	}

	if !strings.Contains(guestInitScriptTemplate, ") &") {
		t.Fatal("expected stdio guest-agent loop to run in background")
	}
}

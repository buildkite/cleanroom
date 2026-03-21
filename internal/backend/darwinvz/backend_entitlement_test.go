//go:build darwin

package darwinvz

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestHasVirtualizationEntitlementHandlesFormattedXML(t *testing.T) {
	raw := `Executable=/tmp/helper
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>com.apple.security.virtualization</key>
    <true/>
  </dict>
</plist>`
	if !hasVirtualizationEntitlement(raw) {
		t.Fatal("expected virtualization entitlement to be detected")
	}
}

func TestHasVirtualizationEntitlementHandlesCompactXML(t *testing.T) {
	raw := `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>`
	if !hasVirtualizationEntitlement(raw) {
		t.Fatal("expected virtualization entitlement to be detected")
	}
}

func TestHasVirtualizationEntitlementRejectsMissingOrFalseEntitlement(t *testing.T) {
	missing := `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.security.app-sandbox</key><true/></dict></plist>`
	if hasVirtualizationEntitlement(missing) {
		t.Fatal("expected missing virtualization entitlement to be rejected")
	}

	falseValue := `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.security.virtualization</key><false/></dict></plist>`
	if hasVirtualizationEntitlement(falseValue) {
		t.Fatal("expected false virtualization entitlement to be rejected")
	}
}

func TestHasVMNetworkingEntitlementHandlesFormattedXML(t *testing.T) {
	raw := `Executable=/tmp/helper
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>com.apple.developer.networking.vmnet</key>
    <true/>
  </dict>
</plist>`
	if !hasVMNetworkingEntitlement(raw) {
		t.Fatal("expected vm networking entitlement to be detected")
	}
}

func TestHasVMNetworkingEntitlementRejectsMissingOrFalseEntitlement(t *testing.T) {
	missing := `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>`
	if hasVMNetworkingEntitlement(missing) {
		t.Fatal("expected missing vm networking entitlement to be rejected")
	}

	falseValue := `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.developer.networking.vmnet</key><false/></dict></plist>`
	if hasVMNetworkingEntitlement(falseValue) {
		t.Fatal("expected false vm networking entitlement to be rejected")
	}

	legacyKey := `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.vm.networking</key><true/></dict></plist>`
	if hasVMNetworkingEntitlement(legacyKey) {
		t.Fatal("expected legacy vm networking entitlement key to be rejected")
	}
}

func TestDoctorVMNetEntitlementResultPassesWhenEntitlementPresent(t *testing.T) {
	status, message := doctorVMNetEntitlementResult("/tmp/helper", true, nil)
	if status != "pass" {
		t.Fatalf("unexpected status: got %q want %q", status, "pass")
	}
	if !strings.Contains(message, "includes com.apple.developer.networking.vmnet entitlement") {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestDoctorVMNetEntitlementResultWarnsWhenEntitlementMissing(t *testing.T) {
	status, message := doctorVMNetEntitlementResult("/tmp/helper", false, nil)
	if status != "warn" {
		t.Fatalf("unexpected status: got %q want %q", status, "warn")
	}
	if !strings.Contains(message, "unsandboxed local builds may still work with vmnet-shared") {
		t.Fatalf("unexpected message: %q", message)
	}
	if !strings.Contains(message, "entitlements-vmnet.plist") {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestDoctorVMNetEntitlementResultWarnsWhenVerificationFails(t *testing.T) {
	errSentinel := errors.New("boom")
	status, message := doctorVMNetEntitlementResult("/tmp/helper", false, errSentinel)
	if status != "warn" {
		t.Fatalf("unexpected status: got %q want %q", status, "warn")
	}
	if !strings.Contains(message, "could not verify com.apple.developer.networking.vmnet entitlement") {
		t.Fatalf("unexpected message: %q", message)
	}
	if !strings.Contains(message, errSentinel.Error()) {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestDoctorWarnsWhenVMNetEntitlementIsMissing(t *testing.T) {
	t.Setenv(helperEnvVar, "/usr/bin/true")

	report, err := New().Doctor(context.Background(), backend.DoctorRequest{
		FirecrackerConfig: backend.FirecrackerConfig{
			DarwinVZNetworkMode: darwinVZNetworkModeVMNetShared,
		},
	})
	if err != nil {
		t.Fatalf("Doctor returned error: %v", err)
	}

	var vmnetCheck *backend.DoctorCheck
	for i := range report.Checks {
		if report.Checks[i].Name == "vmnet_entitlement" {
			vmnetCheck = &report.Checks[i]
			break
		}
	}
	if vmnetCheck == nil {
		t.Fatalf("expected vmnet_entitlement check in report: %#v", report.Checks)
	}
	if got, want := vmnetCheck.Status, "warn"; got != want {
		t.Fatalf("unexpected vmnet_entitlement status: got %q want %q (message: %q)", got, want, vmnetCheck.Message)
	}
	if !strings.Contains(vmnetCheck.Message, "unsandboxed local builds may still work with vmnet-shared") {
		t.Fatalf("unexpected vmnet_entitlement message: %q", vmnetCheck.Message)
	}
}

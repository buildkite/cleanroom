//go:build darwin

package darwinvz

import "testing"

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

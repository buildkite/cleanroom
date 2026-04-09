package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestInstallGlobalRequiresDarwinHelperAppBundle(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("install-global.sh")
	if err != nil {
		t.Fatalf("read install-global.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		`install_app_bundle() {`,
		`HELPER_APP_BUNDLE="${DIST_DIR}/cleanroom-darwin-vz.app"`,
		`require_cmd ditto`,
		`install_app_bundle "$HELPER_APP_BUNDLE" "${INSTALL_DIR}/cleanroom-darwin-vz.app"`,
		`log "installed cleanroom-darwin-vz.app to ${INSTALL_DIR}/cleanroom-darwin-vz.app"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected install-global.sh to contain %q", needle)
		}
	}

	for _, needle := range []string{
		`require_cmd codesign`,
		`install_binary "$HELPER_BIN" "${INSTALL_DIR}/cleanroom-darwin-vz"`,
	} {
		if strings.Contains(script, needle) {
			t.Fatalf("expected install-global.sh not to contain %q", needle)
		}
	}
}

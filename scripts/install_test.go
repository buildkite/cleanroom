package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestInstallScriptIsSingleBinary(t *testing.T) {
	content, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	script := string(content)

	for _, legacy := range []string{
		"guest-agent",
		"darwin-vz",
		"install_macos_pkg",
		"install_cleanroom_daemon",
		".pkg",
	} {
		if strings.Contains(script, legacy) {
			t.Fatalf("install.sh still references removed runtime component %q", legacy)
		}
	}

	for _, want := range []string{
		`asset="cleanroom_${OS}_${ARCH}.tar.gz"`,
		"checksums.txt",
		`did not contain the cleanroom binary`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install.sh missing expected single-binary logic %q", want)
		}
	}
}

package minimal_test

import (
	"os"
	"strings"
	"testing"
)

func TestBuildKernelVerifiesCachedSourceWithChecksumStamp(t *testing.T) {
	raw, err := os.ReadFile("build-kernel.sh")
	if err != nil {
		t.Fatalf("read build-kernel.sh: %v", err)
	}
	script := string(raw)

	required := []string{
		`source_stamp="${src}.source.sha256"`,
		`[[ ! -d "${src}" || ! -f "${source_stamp}" || "$(<"${source_stamp}")" != "${KERNEL_TARBALL_SHA256}" ]]`,
		`rm -rf "${src}" "${source_stamp}"`,
		`echo "${KERNEL_TARBALL_SHA256}  ${tarball}" | sha256sum -c -`,
		`printf "%s\n" "${KERNEL_TARBALL_SHA256}" > "${source_stamp}"`,
	}
	for _, want := range required {
		if !strings.Contains(script, want) {
			t.Fatalf("expected build-kernel.sh to contain %q", want)
		}
	}
}

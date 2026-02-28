package imagemgr

import "testing"

func TestLinuxPlatformForArch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		goArch      string
		wantOS      string
		wantArch    string
		wantVariant string
	}{
		{name: "amd64", goArch: "amd64", wantOS: "linux", wantArch: "amd64", wantVariant: ""},
		{name: "arm64", goArch: "arm64", wantOS: "linux", wantArch: "arm64", wantVariant: "v8"},
		{name: "fallback", goArch: "ppc64le", wantOS: "linux", wantArch: "ppc64le", wantVariant: ""},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := linuxPlatformForArch(tc.goArch)
			if got.OS != tc.wantOS {
				t.Fatalf("unexpected OS: got %q want %q", got.OS, tc.wantOS)
			}
			if got.Architecture != tc.wantArch {
				t.Fatalf("unexpected architecture: got %q want %q", got.Architecture, tc.wantArch)
			}
			if got.Variant != tc.wantVariant {
				t.Fatalf("unexpected variant: got %q want %q", got.Variant, tc.wantVariant)
			}
		})
	}
}

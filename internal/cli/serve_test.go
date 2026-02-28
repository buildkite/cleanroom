package cli

import "testing"

func TestShouldInstallGatewayFirewall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		goos string
		want bool
	}{
		{name: "linux", goos: "linux", want: true},
		{name: "linux case-insensitive", goos: "LiNuX", want: true},
		{name: "darwin", goos: "darwin", want: false},
		{name: "windows", goos: "windows", want: false},
		{name: "whitespace", goos: "  linux  ", want: true},
		{name: "empty", goos: "", want: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldInstallGatewayFirewall(tc.goos); got != tc.want {
				t.Fatalf("shouldInstallGatewayFirewall(%q) = %v, want %v", tc.goos, got, tc.want)
			}
		})
	}
}

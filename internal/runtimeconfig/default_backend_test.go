package runtimeconfig

import "testing"

func TestDefaultBackendForGOOS(t *testing.T) {
	tests := []struct {
		name string
		goos string
		want string
	}{
		{name: "darwin", goos: "darwin", want: "darwin-vz"},
		{name: "darwin case insensitive", goos: "Darwin", want: "darwin-vz"},
		{name: "linux", goos: "linux", want: "firecracker"},
		{name: "windows", goos: "windows", want: "firecracker"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultBackendForGOOS(tt.goos); got != tt.want {
				t.Fatalf("DefaultBackendForGOOS(%q) = %q, want %q", tt.goos, got, tt.want)
			}
		})
	}
}

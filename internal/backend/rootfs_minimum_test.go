package backend

import "testing"

func TestEffectiveRootFSMinimumSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  FirecrackerConfig
		want string
	}{
		{
			name: "unset when no floor",
			cfg: FirecrackerConfig{
				MinimumRootFSBytesSource: RootFSMinimumSourcePolicy,
			},
			want: RootFSMinimumSourceUnset,
		},
		{
			name: "unknown when floor has no source",
			cfg: FirecrackerConfig{
				MinimumRootFSBytes: 8 << 30,
			},
			want: RootFSMinimumSourceUnknown,
		},
		{
			name: "configured source",
			cfg: FirecrackerConfig{
				MinimumRootFSBytes:       8 << 30,
				MinimumRootFSBytesSource: RootFSMinimumSourceConfig,
			},
			want: RootFSMinimumSourceConfig,
		},
		{
			name: "trimmed source",
			cfg: FirecrackerConfig{
				MinimumRootFSBytes:       8 << 30,
				MinimumRootFSBytesSource: " " + RootFSMinimumSourcePolicy + " ",
			},
			want: RootFSMinimumSourcePolicy,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := EffectiveRootFSMinimumSource(tt.cfg); got != tt.want {
				t.Fatalf("EffectiveRootFSMinimumSource() = %q, want %q", got, tt.want)
			}
		})
	}
}

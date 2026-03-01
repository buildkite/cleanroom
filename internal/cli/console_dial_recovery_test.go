package cli

import (
	"errors"
	"testing"
)

func TestResolveConsoleDialFailure(t *testing.T) {
	t.Parallel()

	dialErr := errors.New("dial failed")
	tests := []struct {
		name       string
		replayFn   func() (int, bool, error)
		getFinalFn func() (int, bool)
		wantErr    bool
		wantCode   int
	}{
		{
			name: "replay reports success",
			replayFn: func() (int, bool, error) {
				return 0, true, nil
			},
			getFinalFn: func() (int, bool) { return 0, false },
		},
		{
			name: "replay reports non-zero exit",
			replayFn: func() (int, bool, error) {
				return 7, true, nil
			},
			getFinalFn: func() (int, bool) { return 0, false },
			wantErr:    true,
			wantCode:   7,
		},
		{
			name: "replay has no exit but final status succeeds",
			replayFn: func() (int, bool, error) {
				return 0, false, nil
			},
			getFinalFn: func() (int, bool) { return 0, true },
		},
		{
			name: "replay errors but final status has exit",
			replayFn: func() (int, bool, error) {
				return 0, false, errors.New("stream unavailable")
			},
			getFinalFn: func() (int, bool) { return 9, true },
			wantErr:    true,
			wantCode:   9,
		},
		{
			name: "replay has no exit and final status unavailable",
			replayFn: func() (int, bool, error) {
				return 0, false, nil
			},
			getFinalFn: func() (int, bool) { return 0, false },
			wantErr:    true,
			wantCode:   -1,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := resolveConsoleDialFailure(dialErr, tc.replayFn, tc.getFinalFn)
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr {
				return
			}
			if tc.wantCode >= 0 {
				if got := ExitCode(err); got != tc.wantCode {
					t.Fatalf("unexpected exit code: got %d want %d (err=%v)", got, tc.wantCode, err)
				}
				return
			}
			if !errors.Is(err, dialErr) {
				t.Fatalf("expected wrapped dial error, got %v", err)
			}
		})
	}
}

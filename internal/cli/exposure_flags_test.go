package cli

import (
	"strings"
	"testing"
)

func TestParseExposureFlags(t *testing.T) {
	t.Parallel()

	exposures, err := parseExposureFlags(
		[]string{"5432", "15432:5432"},
		[]string{"buildkite:3000", "*.buildkite:3001", "3002"},
	)
	if err != nil {
		t.Fatalf("parseExposureFlags returned error: %v", err)
	}
	if got, want := len(exposures), 5; got != want {
		t.Fatalf("unexpected exposure count: got %d want %d", got, want)
	}
	if got := exposures[0]; got.GetProtocol() != exposureProtocolTCP || got.GetHostPort() != 5432 || got.GetGuestPort() != 5432 {
		t.Fatalf("unexpected first exposure: %#v", got)
	}
	if got := exposures[1]; got.GetProtocol() != exposureProtocolTCP || got.GetHostPort() != 15432 || got.GetGuestPort() != 5432 {
		t.Fatalf("unexpected second exposure: %#v", got)
	}
	if got := exposures[2]; got.GetProtocol() != exposureProtocolHTTPS || got.GetName() != "buildkite" || got.GetGuestPort() != 3000 {
		t.Fatalf("unexpected third exposure: %#v", got)
	}
	if got := exposures[3]; got.GetProtocol() != exposureProtocolHTTPS || got.GetName() != "*.buildkite" || got.GetGuestPort() != 3001 {
		t.Fatalf("unexpected fourth exposure: %#v", got)
	}
	if got := exposures[4]; got.GetProtocol() != exposureProtocolHTTPS || got.GetName() != "" || got.GetGuestPort() != 3002 {
		t.Fatalf("unexpected fifth exposure: %#v", got)
	}
}

func TestParseExposureFlagsRejectsInvalidSpecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tcpSpecs  []string
		httpsSpec []string
		want      string
	}{
		{name: "tcp empty", tcpSpecs: []string{""}, want: "empty exposure"},
		{name: "tcp too many parts", tcpSpecs: []string{"a:b:c"}, want: "expected <guest-port>"},
		{name: "tcp bad port", tcpSpecs: []string{"buildkite:5432"}, want: "host port"},
		{name: "tcp out of range", tcpSpecs: []string{"70000"}, want: "out of range"},
		{name: "https empty", httpsSpec: []string{""}, want: "empty exposure"},
		{name: "https bad name", httpsSpec: []string{"Buildkite:3000"}, want: "lowercase"},
		{name: "https explicit dotted exact name", httpsSpec: []string{"api.buildkite:3000"}, want: "exact single label or a leading wildcard"},
		{name: "https empty dotted label", httpsSpec: []string{"api..buildkite:3000"}, want: "empty DNS label"},
		{name: "https trailing dot", httpsSpec: []string{"buildkite.:3000"}, want: "empty DNS label"},
		{name: "https bare wildcard", httpsSpec: []string{"*:3000"}, want: "must contain only lowercase"},
		{name: "https missing wildcard suffix", httpsSpec: []string{"*.:3000"}, want: "wildcard suffix"},
		{name: "https wildcard in middle", httpsSpec: []string{"foo.*.buildkite:3000"}, want: "exact single label or a leading wildcard"},
		{name: "https wildcard uppercase suffix", httpsSpec: []string{"*.API.buildkite:3000"}, want: "lowercase"},
		{name: "https too many parts", httpsSpec: []string{"buildkite:https:3000"}, want: "expected [name:]<guest-port>"},
		{name: "https bad port", httpsSpec: []string{"buildkite:port"}, want: "port must be numeric"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseExposureFlags(tc.tcpSpecs, tc.httpsSpec)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestNormalizeBareExposeHTTPSArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "bare before command separator",
			args: []string{"exec", "--expose-https", "--", "npm", "run", "dev"},
			want: []string{"exec", "--expose-https=" + configuredHTTPSExposureSpec, "--", "npm", "run", "dev"},
		},
		{
			name: "bare at end",
			args: []string{"create", "--expose-https"},
			want: []string{"create", "--expose-https=" + configuredHTTPSExposureSpec},
		},
		{
			name: "explicit value",
			args: []string{"create", "--expose-https", "buildkite:3000"},
			want: []string{"create", "--expose-https", "buildkite:3000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeBareExposeHTTPSArgs(tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unexpected args: got %v want %v", got, tt.want)
			}
			if len(tt.args) > 0 && &got[0] == &tt.args[0] {
				t.Fatal("expected normalized args to be a copy")
			}
		})
	}
}

func TestExpandConfiguredHTTPSExposures(t *testing.T) {
	t.Parallel()

	got, err := expandConfiguredHTTPSExposures(policy.ExposeHTTPSConfig{
		Base: "{sandbox_id}.localhost",
		Routes: []policy.ExposeHTTPSRoute{{
			Port:  3000,
			Hosts: []string{"{base}", "*.{base}", "*.*.{base}"},
		}},
	}, "Cr-123")
	if err != nil {
		t.Fatalf("expandConfiguredHTTPSExposures returned error: %v", err)
	}

	want := []*cleanroomv1.PortExposure{
		{Protocol: exposureProtocolHTTPS, GuestPort: 3000, Name: "cr-123.localhost"},
		{Protocol: exposureProtocolHTTPS, GuestPort: 3000, Name: "*.cr-123.localhost"},
		{Protocol: exposureProtocolHTTPS, GuestPort: 3000, Name: "*.*.cr-123.localhost"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected exposures: got %v want %v", got, want)
	}
}

func TestResolveRequestedExposuresLoadsConfiguredHTTPS(t *testing.T) {
	t.Parallel()

	loader := &configuredExposureLoader{
		cfg: policy.ExposeConfig{HTTPS: policy.ExposeHTTPSConfig{
			Base: "{container_id}.localhost",
			Routes: []policy.ExposeHTTPSRoute{{
				Port:  3000,
				Hosts: []string{"{base}", "*.{base}"},
			}},
		}},
	}
	got, err := resolveRequestedExposures(&runtimeContext{Loader: loader}, "/repo", "sandbox-1", []*cleanroomv1.PortExposure{
		{Protocol: exposureProtocolTCP, HostPort: 5432, GuestPort: 5432},
		{Protocol: exposureProtocolHTTPS, Name: configuredHTTPSExposureSpec},
	})
	if err != nil {
		t.Fatalf("resolveRequestedExposures returned error: %v", err)
	}
	if got, want := loader.cwd, "/repo"; got != want {
		t.Fatalf("unexpected loader cwd: got %q want %q", got, want)
	}
	if got, want := len(got), 3; got != want {
		t.Fatalf("unexpected exposure count: got %d want %d", got, want)
	}
	if got, want := got[1].GetName(), "sandbox-1.localhost"; got != want {
		t.Fatalf("unexpected first configured host: got %q want %q", got, want)
	}
	if got, want := got[2].GetName(), "*.sandbox-1.localhost"; got != want {
		t.Fatalf("unexpected second configured host: got %q want %q", got, want)
	}
}

func TestResolveRequestedExposuresRequiresConfiguredHTTPS(t *testing.T) {
	t.Parallel()

	_, err := resolveRequestedExposures(&runtimeContext{Loader: &configuredExposureLoader{}}, "/repo", "sandbox-1", []*cleanroomv1.PortExposure{
		{Protocol: exposureProtocolHTTPS, Name: configuredHTTPSExposureSpec},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "requires expose.https") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrevalidateConfiguredExposuresRequiresConfiguredHTTPS(t *testing.T) {
	t.Parallel()

	loader := &configuredExposureLoader{}
	err := prevalidateConfiguredExposures(&runtimeContext{Loader: loader}, "/repo", []*cleanroomv1.PortExposure{
		{Protocol: exposureProtocolHTTPS, Name: configuredHTTPSExposureSpec},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "requires expose.https") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := loader.cwd, "/repo"; got != want {
		t.Fatalf("unexpected loader cwd: got %q want %q", got, want)
	}
}

type configuredExposureLoader struct {
	cfg policy.ExposeConfig
	cwd string
}

func (l *configuredExposureLoader) LoadAndCompile(string) (*policy.CompiledPolicy, string, error) {
	return nil, "", errors.New("LoadAndCompile should not be called")
}

func (l *configuredExposureLoader) LoadRepository(string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "", errors.New("LoadRepository should not be called")
}

func (l *configuredExposureLoader) LoadExpose(cwd string) (policy.ExposeConfig, string, error) {
	l.cwd = cwd
	return l.cfg, "/repo/cleanroom.yaml", nil
}

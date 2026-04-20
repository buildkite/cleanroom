//go:build linux

package main

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/gateway"
)

func TestBuildCommandEnvAppliesGatewayDefaultsWhenUnset(t *testing.T) {
	t.Setenv("GOPROXY", "")
	t.Setenv("MISE_GO_DOWNLOAD_MIRROR", "")

	env, err := buildCommandEnv([]string{
		gateway.GoProxyDefaultEnvKey + "=http://gateway.cleanroom.internal:8170/goproxy,direct",
		gateway.MiseGoDownloadMirrorDefaultEnvKey + "=http://gateway.cleanroom.internal:8170/fetch/dl.google.com/go",
	})
	if err != nil {
		t.Fatalf("buildCommandEnv returned error: %v", err)
	}

	got := envMap(env)
	if got["GOPROXY"] != "http://gateway.cleanroom.internal:8170/goproxy,direct" {
		t.Fatalf("expected GOPROXY default to be applied, got %q", got["GOPROXY"])
	}
	if got["MISE_GO_DOWNLOAD_MIRROR"] != "http://gateway.cleanroom.internal:8170/fetch/dl.google.com/go" {
		t.Fatalf("expected MISE_GO_DOWNLOAD_MIRROR default to be applied, got %q", got["MISE_GO_DOWNLOAD_MIRROR"])
	}
	if _, ok := got[gateway.GoProxyDefaultEnvKey]; ok {
		t.Fatalf("did not expect %s to remain in child env", gateway.GoProxyDefaultEnvKey)
	}
	if _, ok := got[gateway.MiseGoDownloadMirrorDefaultEnvKey]; ok {
		t.Fatalf("did not expect %s to remain in child env", gateway.MiseGoDownloadMirrorDefaultEnvKey)
	}
}

func TestBuildCommandEnvPreservesExplicitUserGoEnv(t *testing.T) {
	t.Setenv("GOPROXY", "")
	t.Setenv("MISE_GO_DOWNLOAD_MIRROR", "")

	env, err := buildCommandEnv([]string{
		"GOPROXY=off",
		"MISE_GO_DOWNLOAD_MIRROR=https://example.test/go",
		gateway.GoProxyDefaultEnvKey + "=http://gateway.cleanroom.internal:8170/goproxy,direct",
		gateway.MiseGoDownloadMirrorDefaultEnvKey + "=http://gateway.cleanroom.internal:8170/fetch/dl.google.com/go",
	})
	if err != nil {
		t.Fatalf("buildCommandEnv returned error: %v", err)
	}

	got := envMap(env)
	if got["GOPROXY"] != "off" {
		t.Fatalf("expected explicit GOPROXY to win, got %q", got["GOPROXY"])
	}
	if got["MISE_GO_DOWNLOAD_MIRROR"] != "https://example.test/go" {
		t.Fatalf("expected explicit MISE_GO_DOWNLOAD_MIRROR to win, got %q", got["MISE_GO_DOWNLOAD_MIRROR"])
	}
	if _, ok := got[gateway.GoProxyDefaultEnvKey]; ok {
		t.Fatalf("did not expect %s to remain in child env", gateway.GoProxyDefaultEnvKey)
	}
	if _, ok := got[gateway.MiseGoDownloadMirrorDefaultEnvKey]; ok {
		t.Fatalf("did not expect %s to remain in child env", gateway.MiseGoDownloadMirrorDefaultEnvKey)
	}
}

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			out[entry] = ""
			continue
		}
		out[key] = value
	}
	return out
}

//go:build linux

package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/guestenv"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestBuildCommandEnvAppliesGatewayDefaultsWhenUnset(t *testing.T) {
	t.Setenv("GOPROXY", "")
	t.Setenv("MISE_GO_DOWNLOAD_MIRROR", "")

	env, err := buildCommandEnv([]string{
		gateway.GoProxyDefaultEnvKey + "=http://gateway.cleanroom.internal:8170/goproxy,direct",
		gateway.MiseGoDownloadMirrorDefaultEnvKey + "=http://gateway.cleanroom.internal:8170/fetch/dl.google.com/go",
	}, true)
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
	}, true)
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

func TestBuildCommandEnvCanDisableAmbientInheritance(t *testing.T) {
	t.Setenv("UNKEYED_AMBIENT", "leak")

	env, err := buildCommandEnv([]string{"DECLARED=value"}, false)
	if err != nil {
		t.Fatalf("buildCommandEnv returned error: %v", err)
	}

	got := envMap(env)
	if got["UNKEYED_AMBIENT"] != "" {
		t.Fatalf("did not expect ambient env to be inherited, got %q", got["UNKEYED_AMBIENT"])
	}
	if got["DECLARED"] != "value" {
		t.Fatalf("expected declared env to be present, got %q", got["DECLARED"])
	}
	if got["HOME"] != guestenv.DefaultHome {
		t.Fatalf("expected default HOME, got %q", got["HOME"])
	}
	if got["PATH"] != guestenv.DefaultPath {
		t.Fatalf("expected default PATH, got %q", got["PATH"])
	}
}

func TestSendErrorResponseStreamsStderrAndExit(t *testing.T) {
	var buf bytes.Buffer

	sendErrorResponse(&buf, errors.New("exec: missing binary"))

	var stderr bytes.Buffer
	res, err := vsockexec.DecodeStreamResponse(&buf, vsockexec.StreamCallbacks{
		OnStderr: func(chunk []byte) { stderr.Write(chunk) },
	})
	if err != nil {
		t.Fatalf("DecodeStreamResponse returned error: %v", err)
	}
	if got, want := stderr.String(), "exec: missing binary\n"; got != want {
		t.Fatalf("unexpected streamed stderr: got %q want %q", got, want)
	}
	if got, want := res.ExitCode, 1; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if got, want := res.Error, "exec: missing binary"; got != want {
		t.Fatalf("unexpected error: got %q want %q", got, want)
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

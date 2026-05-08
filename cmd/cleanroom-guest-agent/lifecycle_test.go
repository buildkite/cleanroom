//go:build linux

package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestRunConnPreludeOrdersSetupBeforeDockerStart(t *testing.T) {
	var calls []string
	origSetup := setupCacheOutputMountsOnceFn
	origDocker := startDockerServiceOnceFn
	t.Cleanup(func() {
		setupCacheOutputMountsOnceFn = origSetup
		startDockerServiceOnceFn = origDocker
	})

	setupCacheOutputMountsOnceFn = func(_ []vsockexec.CacheOutputMount) error {
		calls = append(calls, "setup")
		return nil
	}
	startDockerServiceOnceFn = func() error {
		calls = append(calls, "docker")
		return nil
	}

	if err := runConnPrelude(vsockexec.ExecRequest{StartDockerService: true}); err != nil {
		t.Fatalf("runConnPrelude returned error: %v", err)
	}
	if got, want := calls, []string{"setup", "docker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("call order mismatch: got %v want %v", got, want)
	}
}

func TestRunConnPreludeReturnsSetupErrorBeforeDockerStart(t *testing.T) {
	var dockerCalled bool
	origSetup := setupCacheOutputMountsOnceFn
	origDocker := startDockerServiceOnceFn
	t.Cleanup(func() {
		setupCacheOutputMountsOnceFn = origSetup
		startDockerServiceOnceFn = origDocker
	})

	setupErr := errors.New("setup boom")
	setupCacheOutputMountsOnceFn = func(_ []vsockexec.CacheOutputMount) error {
		return setupErr
	}
	startDockerServiceOnceFn = func() error {
		dockerCalled = true
		return nil
	}

	err := runConnPrelude(vsockexec.ExecRequest{})
	if !errors.Is(err, setupErr) {
		t.Fatalf("expected setup error, got %v", err)
	}
	if dockerCalled {
		t.Fatal("docker start should not run when mount setup fails")
	}
}

func TestRunConnPreludeSkipsDockerStartUnlessRequested(t *testing.T) {
	var calls []string
	origSetup := setupCacheOutputMountsOnceFn
	origDocker := startDockerServiceOnceFn
	t.Cleanup(func() {
		setupCacheOutputMountsOnceFn = origSetup
		startDockerServiceOnceFn = origDocker
	})

	setupCacheOutputMountsOnceFn = func(_ []vsockexec.CacheOutputMount) error {
		calls = append(calls, "setup")
		return nil
	}
	startDockerServiceOnceFn = func() error {
		calls = append(calls, "docker")
		return nil
	}

	if err := runConnPrelude(vsockexec.ExecRequest{}); err != nil {
		t.Fatalf("runConnPrelude returned error: %v", err)
	}
	if got, want := calls, []string{"setup"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("call order mismatch: got %v want %v", got, want)
	}
}

func TestDockerServiceConfigForRequestedStartForcesRequiredFromRequest(t *testing.T) {
	cfg := dockerServiceConfigForRequestedStart("cleanroom_service_docker_required=0")
	if !cfg.Required {
		t.Fatal("expected request-scoped docker startup to override disabled boot arg")
	}
}

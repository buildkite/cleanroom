package backend

import (
	"errors"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/ext4edit"
)

func TestValidateDockerServiceRootFSSkipsWhenDockerNotRequired(t *testing.T) {
	t.Parallel()

	called := false
	err := validateDockerServiceRootFS("rootfs.ext4", "example/image", false, func(string, string) (ext4edit.PathKind, error) {
		called = true
		return ext4edit.PathKindUnknown, nil
	})
	if err != nil {
		t.Fatalf("ValidateDockerServiceRootFS returned error: %v", err)
	}
	if called {
		t.Fatal("did not expect rootfs inspection when docker is not required")
	}
}

func TestValidateDockerServiceRootFSAcceptsDockerdOnPath(t *testing.T) {
	t.Parallel()

	err := validateDockerServiceRootFS("rootfs.ext4", "example/image", true, func(_, path string) (ext4edit.PathKind, error) {
		if path == "/usr/local/sbin/dockerd" {
			return ext4edit.PathKindRegular, nil
		}
		return ext4edit.PathKindUnknown, nil
	})
	if err != nil {
		t.Fatalf("ValidateDockerServiceRootFS returned error: %v", err)
	}
}

func TestValidateDockerServiceRootFSAcceptsSymlinkedDockerdOnPath(t *testing.T) {
	t.Parallel()

	err := validateDockerServiceRootFS("rootfs.ext4", "example/image", true, func(_, path string) (ext4edit.PathKind, error) {
		if path == "/usr/local/bin/dockerd" {
			return ext4edit.PathKindSymlink, nil
		}
		return ext4edit.PathKindUnknown, nil
	})
	if err != nil {
		t.Fatalf("ValidateDockerServiceRootFS returned error: %v", err)
	}
}

func TestValidateDockerServiceRootFSRejectsMissingDockerd(t *testing.T) {
	t.Parallel()

	err := validateDockerServiceRootFS("rootfs.ext4", "ghcr.io/buildkite/cleanroom-base/alpine", true, func(string, string) (ext4edit.PathKind, error) {
		return ext4edit.PathKindUnknown, nil
	})
	if err == nil {
		t.Fatal("expected error when docker is required but dockerd is missing")
	}
	for _, want := range []string{
		"sandbox.docker.required is true",
		`ghcr.io/buildkite/cleanroom-base/alpine`,
		"dockerd",
		"ghcr.io/buildkite/cleanroom-base/debian-docker",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %q", want, err.Error())
		}
	}
}

func TestValidateDockerServiceRootFSPreservesInspectionErrors(t *testing.T) {
	t.Parallel()

	inspectErr := errors.New("debugfs unavailable")
	err := validateDockerServiceRootFS("rootfs.ext4", "example/image", true, func(string, string) (ext4edit.PathKind, error) {
		return ext4edit.PathKindUnknown, inspectErr
	})
	if err == nil {
		t.Fatal("expected inspection error")
	}
	for _, want := range []string{
		"inspect rootfs",
		"debugfs unavailable",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %q", want, err.Error())
		}
	}
}

func TestValidateDockerServiceRootFSRejectsDockerdDirectory(t *testing.T) {
	t.Parallel()

	err := validateDockerServiceRootFS("rootfs.ext4", "example/image", true, func(_, path string) (ext4edit.PathKind, error) {
		if path == "/usr/local/sbin/dockerd" {
			return ext4edit.PathKindDirectory, nil
		}
		return ext4edit.PathKindUnknown, nil
	})
	if err == nil {
		t.Fatal("expected directory placeholder to be rejected")
	}
	if !strings.Contains(err.Error(), "does not contain dockerd in PATH") {
		t.Fatalf("unexpected error: %v", err)
	}
}

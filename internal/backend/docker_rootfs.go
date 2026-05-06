package backend

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/ext4edit"
)

var dockerServiceDockerdPaths = []string{
	"/usr/local/sbin/dockerd",
	"/usr/local/bin/dockerd",
	"/usr/bin/dockerd",
	"/usr/sbin/dockerd",
	"/sbin/dockerd",
	"/bin/dockerd",
}

// ValidateDockerServiceRootFS fails fast when a Docker service policy is paired
// with an image that cannot start dockerd from the guest's default PATH.
func ValidateDockerServiceRootFS(rootFSPath, imageRef string, required bool) error {
	return validateDockerServiceRootFS(rootFSPath, imageRef, required, ext4PathKind)
}

func validateDockerServiceRootFS(rootFSPath, imageRef string, required bool, pathKind func(string, string) (ext4edit.PathKind, error)) error {
	if !required {
		return nil
	}
	if pathKind == nil {
		return fmt.Errorf("sandbox.docker.required is true, but the selected sandbox image cannot be inspected for dockerd")
	}
	for _, path := range dockerServiceDockerdPaths {
		kind, err := pathKind(rootFSPath, path)
		if err != nil {
			return fmt.Errorf("inspect rootfs for required docker service path %q: %w", path, err)
		}
		if kind == ext4edit.PathKindRegular {
			return nil
		}
	}

	imageLabel := "the selected sandbox image"
	if trimmed := strings.TrimSpace(imageRef); trimmed != "" {
		imageLabel = fmt.Sprintf("sandbox image %q", trimmed)
	}
	return fmt.Errorf(
		"sandbox.docker.required is true, but %s does not contain dockerd in PATH (%s); use a Docker-capable image such as ghcr.io/buildkite/cleanroom-base/debian-docker or disable sandbox.docker.required",
		imageLabel,
		strings.Join(dockerServiceDockerdPaths, ", "),
	)
}

func ext4PathKind(rootFSPath, path string) (ext4edit.PathKind, error) {
	return ext4edit.PathTypeWithError(rootFSPath, path)
}

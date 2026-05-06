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
	return validateDockerServiceRootFS(rootFSPath, imageRef, required, ext4edit.PathIsExecutable)
}

func validateDockerServiceRootFS(rootFSPath, imageRef string, required bool, pathIsExecutable func(string, string) (bool, error)) error {
	if !required {
		return nil
	}
	if pathIsExecutable == nil {
		return fmt.Errorf("sandbox.docker.required is true, but the selected sandbox image cannot be inspected for dockerd")
	}
	for _, path := range dockerServiceDockerdPaths {
		executable, err := pathIsExecutable(rootFSPath, path)
		if err != nil {
			return fmt.Errorf("inspect rootfs for required docker service path %q: %w", path, err)
		}
		if executable {
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

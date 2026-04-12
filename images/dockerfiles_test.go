package images_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const expectedMiseVersion = "v2026.4.5"

func publishedBaseDockerfiles() []string {
	return []string{
		"Dockerfile.base-image",
		"Dockerfile.base-image-docker",
		"Dockerfile.base-image-agents",
	}
}

func debianPublishedBaseDockerfiles() []string {
	return []string{
		"Dockerfile.base-image-debian",
		"Dockerfile.base-image-debian-docker",
		"Dockerfile.base-image-debian-agents",
		"Dockerfile.base-image-debian-ruby",
	}
}

func allPublishedBaseDockerfiles() []string {
	paths := publishedBaseDockerfiles()
	return append(paths, debianPublishedBaseDockerfiles()...)
}

func dockerfileHasTrimmedLine(dockerfile, want string) bool {
	for _, line := range strings.Split(dockerfile, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func TestPublishedBaseImagesInstallBash(t *testing.T) {
	t.Parallel()

	for _, relPath := range allPublishedBaseDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			if !dockerfileHasTrimmedLine(string(raw), "bash \\") {
				t.Fatalf("%s does not install bash", relPath)
			}
		})
	}
}

func TestPublishedBaseImagesInstallOpenSSHClientDefault(t *testing.T) {
	t.Parallel()

	for _, relPath := range publishedBaseDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			if !dockerfileHasTrimmedLine(string(raw), "openssh-client-default \\") {
				t.Fatalf("%s does not install openssh-client-default", relPath)
			}
		})
	}
}

func TestDebianPublishedBaseImagesInstallOpenSSHClient(t *testing.T) {
	t.Parallel()

	for _, relPath := range debianPublishedBaseDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			if !dockerfileHasTrimmedLine(string(raw), "openssh-client \\") {
				t.Fatalf("%s does not install openssh-client", relPath)
			}
		})
	}
}

func TestPublishedBaseImagesInstallPinnedMiseRelease(t *testing.T) {
	t.Parallel()

	for _, relPath := range allPublishedBaseDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}

			dockerfile := string(raw)
			if !strings.Contains(dockerfile, "ARG MISE_VERSION="+expectedMiseVersion) {
				t.Fatalf("%s does not pin mise to %s", relPath, expectedMiseVersion)
			}
			if !dockerfileHasTrimmedLine(dockerfile, "curl \\") {
				t.Fatalf("%s does not install curl for pinned mise bootstrap", relPath)
			}
			if !strings.Contains(dockerfile, "curl -fsSL https://mise.run |") {
				t.Fatalf("%s does not install mise via the official installer", relPath)
			}

			for _, line := range strings.Split(dockerfile, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "mise" || trimmed == "mise \\" {
					t.Fatalf("%s still installs mise from apk", relPath)
				}
			}
		})
	}
}

func TestPublishedBaseImagesExposeMiseShimsOnPATH(t *testing.T) {
	t.Parallel()

	for _, relPath := range allPublishedBaseDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			if !strings.Contains(string(raw), `ENV PATH="/root/.local/share/mise/shims:${PATH}"`) {
				t.Fatalf("%s does not add mise shims to PATH", relPath)
			}
		})
	}
}

func TestPublishedBaseImagesTrustWorkspaceMiseConfig(t *testing.T) {
	t.Parallel()

	for _, relPath := range allPublishedBaseDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			if !strings.Contains(string(raw), "mise settings add trusted_config_paths /workspace") {
				t.Fatalf("%s does not trust /workspace for repo-local mise config", relPath)
			}
		})
	}
}

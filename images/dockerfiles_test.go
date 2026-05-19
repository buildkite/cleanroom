package images_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const expectedMiseVersion = "v2026.4.5"
const expectedPICodingAgentVersion = "0.52.9"

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

func agentDockerfiles() []string {
	return []string{
		"Dockerfile.base-image-agents",
		"Dockerfile.base-image-debian-agents",
	}
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

func TestDebianPublishedBaseImagesInstallIPRoute2(t *testing.T) {
	t.Parallel()

	for _, relPath := range debianPublishedBaseDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			if !dockerfileHasTrimmedLine(string(raw), "iproute2 \\") {
				t.Fatalf("%s does not install iproute2 for darwin-vz guest routing", relPath)
			}
		})
	}
}

func TestDebianRubyBaseImageInstallsLibXML2Dev(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(".", "Dockerfile.base-image-debian-ruby"))
	if err != nil {
		t.Fatalf("read Dockerfile.base-image-debian-ruby: %v", err)
	}
	if !dockerfileHasTrimmedLine(string(raw), "libxml2-dev \\") {
		t.Fatal("Dockerfile.base-image-debian-ruby does not install libxml2-dev for native Ruby gem builds")
	}
}

func TestDebianRubyBaseImageInstallsDefaultLibMySQLClientDev(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(".", "Dockerfile.base-image-debian-ruby"))
	if err != nil {
		t.Fatalf("read Dockerfile.base-image-debian-ruby: %v", err)
	}
	if !dockerfileHasTrimmedLine(string(raw), "default-libmysqlclient-dev \\") {
		t.Fatal("Dockerfile.base-image-debian-ruby does not install default-libmysqlclient-dev for mysql2 native gem builds")
	}
}

func TestDebianBaseImagesInstallLibatomicForMiseManagedNode(t *testing.T) {
	t.Parallel()

	for _, relPath := range debianPublishedBaseDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			if !dockerfileHasTrimmedLine(string(raw), "libatomic1 \\") {
				t.Fatalf("%s does not install libatomic1 for mise-managed Node.js on arm64", relPath)
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

func TestAgentBaseImagesInstallPinnedPICodingAgent(t *testing.T) {
	t.Parallel()

	for _, relPath := range agentDockerfiles() {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}

			dockerfile := string(raw)
			if !strings.Contains(dockerfile, "ARG PI_CODING_AGENT_VERSION="+expectedPICodingAgentVersion) {
				t.Fatalf("%s does not pin pi-coding-agent to %s", relPath, expectedPICodingAgentVersion)
			}
			if !strings.Contains(dockerfile, `npm:@mariozechner/pi-coding-agent@"${PI_CODING_AGENT_VERSION}"`) {
				t.Fatalf("%s does not install pi-coding-agent through mise", relPath)
			}
			if !strings.Contains(dockerfile, "ln -sf /root/.local/share/mise/shims/pi /usr/local/bin/pi") {
				t.Fatalf("%s does not expose pi on the default PATH", relPath)
			}
			if !strings.Contains(dockerfile, `PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" pi --version`) {
				t.Fatalf("%s does not smoke-test pi on the default PATH", relPath)
			}
			if strings.Count(dockerfile, "rm -rf /root/.cache /root/.npm /tmp/*") < 2 {
				t.Fatalf("%s does not remove npm, node-gyp, and temporary build caches after install and smoke checks", relPath)
			}
		})
	}
}

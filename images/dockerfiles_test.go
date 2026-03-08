package images_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedBaseImagesInstallBash(t *testing.T) {
	t.Parallel()

	for _, relPath := range []string{
		"Dockerfile.base-image",
		"Dockerfile.base-image-docker",
		"Dockerfile.base-image-agents",
	} {
		relPath := relPath
		t.Run(relPath, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(".", relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			if !strings.Contains(string(raw), "\n  bash \\\n") {
				t.Fatalf("%s does not install bash", relPath)
			}
		})
	}
}

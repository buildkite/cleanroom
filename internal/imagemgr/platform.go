package imagemgr

import (
	"fmt"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func HostLinuxPlatformForGOARCH(goarch string) v1.Platform {
	switch NormalizePlatformArch(goarch) {
	case "amd64":
		return v1.Platform{OS: "linux", Architecture: "amd64"}
	case "arm64":
		return v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	default:
		return v1.Platform{OS: "linux", Architecture: strings.TrimSpace(goarch)}
	}
}

func ValidateImagePlatformForHost(imageOS, imageArch, hostArch string) error {
	resolvedOS := strings.TrimSpace(strings.ToLower(imageOS))
	if resolvedOS == "" {
		resolvedOS = "linux"
	}
	if resolvedOS != "linux" {
		return fmt.Errorf("image OS %q is unsupported (expected linux)", resolvedOS)
	}

	resolvedImageArch := NormalizePlatformArch(imageArch)
	resolvedHostArch := NormalizePlatformArch(hostArch)
	if resolvedImageArch == "" || resolvedHostArch == "" {
		return nil
	}
	if resolvedImageArch != resolvedHostArch {
		return fmt.Errorf("image architecture linux/%s is incompatible with host guest architecture linux/%s", resolvedImageArch, resolvedHostArch)
	}
	return nil
}

func NormalizePlatformArch(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return strings.TrimSpace(strings.ToLower(raw))
	}
}

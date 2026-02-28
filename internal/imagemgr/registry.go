package imagemgr

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func pullImageFromRegistry(ctx context.Context, ref string) (io.ReadCloser, OCIConfig, error) {
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		return nil, OCIConfig{}, fmt.Errorf("parse digest reference %q: %w", ref, err)
	}

	img, err := remote.Image(digestRef, remote.WithContext(ctx), remote.WithPlatform(hostLinuxPlatform()))
	if err != nil {
		return nil, OCIConfig{}, fmt.Errorf("pull OCI image %q: %w", ref, err)
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, OCIConfig{}, fmt.Errorf("read OCI config for %q: %w", ref, err)
	}

	rootFSTar := mutate.Extract(img)

	return rootFSTar, OCIConfig{
		Entrypoint: append([]string(nil), cfg.Config.Entrypoint...),
		Cmd:        append([]string(nil), cfg.Config.Cmd...),
		Env:        append([]string(nil), cfg.Config.Env...),
		Workdir:    cfg.Config.WorkingDir,
		User:       cfg.Config.User,
	}, nil
}

func hostLinuxPlatform() v1.Platform {
	return linuxPlatformForArch(runtime.GOARCH)
}

func linuxPlatformForArch(goArch string) v1.Platform {
	switch goArch {
	case "amd64":
		return v1.Platform{OS: "linux", Architecture: "amd64"}
	case "arm64":
		return v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	default:
		return v1.Platform{OS: "linux", Architecture: goArch}
	}
}

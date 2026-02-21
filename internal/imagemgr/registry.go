package imagemgr

import (
	"context"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func pullImageFromRegistry(ctx context.Context, ref string) (io.ReadCloser, OCIConfig, error) {
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		return nil, OCIConfig{}, fmt.Errorf("parse digest reference %q: %w", ref, err)
	}

	img, err := remote.Image(digestRef, remote.WithContext(ctx))
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

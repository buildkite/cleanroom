package imagemgr

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func pullImageFromRegistry(resolveCtx, streamCtx context.Context, ref string) (io.ReadCloser, OCIConfig, error) {
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		return nil, OCIConfig{}, fmt.Errorf("parse digest reference %q: %w", ref, err)
	}

	platform := hostLinuxPlatform()
	img, err := remote.Image(digestRef, remote.WithContext(resolveCtx), remote.WithPlatform(platform))
	if err != nil {
		return nil, OCIConfig{}, fmt.Errorf("pull OCI image %q: %w", ref, err)
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, OCIConfig{}, fmt.Errorf("read OCI config for %q: %w", ref, err)
	}

	return newRegistryRootFSStream(streamCtx, digestRef, platform), OCIConfig{
		Entrypoint:   append([]string(nil), cfg.Config.Entrypoint...),
		Cmd:          append([]string(nil), cfg.Config.Cmd...),
		Env:          append([]string(nil), cfg.Config.Env...),
		Workdir:      cfg.Config.WorkingDir,
		User:         cfg.Config.User,
		OS:           cfg.OS,
		Architecture: cfg.Architecture,
		Variant:      cfg.Variant,
	}, nil
}

type registryRootFSStream struct {
	reader *io.PipeReader
	cancel context.CancelFunc
	once   sync.Once
}

func newRegistryRootFSStream(ctx context.Context, digestRef name.Digest, platform v1.Platform) io.ReadCloser {
	streamCtx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	stream := &registryRootFSStream{
		reader: reader,
		cancel: cancel,
	}

	go func() {
		defer cancel()

		img, err := remote.Image(digestRef, remote.WithContext(streamCtx), remote.WithPlatform(platform))
		if err != nil {
			_ = writer.CloseWithError(fmt.Errorf("pull OCI image rootfs %q: %w", digestRef.Name(), err))
			return
		}

		rootFSTar := mutate.Extract(img)
		_, copyErr := io.Copy(writer, rootFSTar)
		closeErr := rootFSTar.Close()
		if copyErr != nil {
			_ = writer.CloseWithError(copyErr)
			return
		}
		if closeErr != nil {
			_ = writer.CloseWithError(closeErr)
			return
		}
		_ = writer.Close()
	}()

	return stream
}

func (s *registryRootFSStream) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}

func (s *registryRootFSStream) Close() error {
	var err error
	s.once.Do(func() {
		s.cancel()
		err = s.reader.Close()
	})
	return err
}

func hostLinuxPlatform() v1.Platform {
	return HostLinuxPlatformForGOARCH(runtime.GOARCH)
}

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/imagemgr"
	"github.com/buildkite/cleanroom/internal/ociref"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var (
	resolveRegistryDigestForReference = func(ctx context.Context, ref name.Reference) (string, error) {
		desc, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
		if err != nil {
			return "", err
		}
		return desc.Digest.String(), nil
	}
	resolveReferencePlatformConfig = func(ctx context.Context, ref name.Reference) (string, string, error) {
		img, err := remote.Image(
			ref,
			remote.WithContext(ctx),
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
			remote.WithPlatform(imagemgr.HostLinuxPlatformForGOARCH(runtime.GOARCH)),
		)
		if err != nil {
			return "", "", err
		}

		cfg, err := img.ConfigFile()
		if err != nil {
			return "", "", err
		}
		return cfg.OS, cfg.Architecture, nil
	}
	resolveReferenceForPolicyUpdate = func(ctx context.Context, source string) (string, error) {
		resolved := strings.TrimSpace(source)
		if resolved == "" {
			resolved = defaultBumpRefSource
		}

		ref, err := name.ParseReference(resolved, name.WeakValidation)
		if err != nil {
			return "", fmt.Errorf("parse image ref %q: %w", resolved, err)
		}
		resolvedDigest, err := resolveRegistryDigestForReference(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("resolve image digest for %q: %w", resolved, err)
		}

		return fmt.Sprintf("%s@%s", ref.Context().Name(), resolvedDigest), nil
	}
	importLocalDockerImageForOverrideFn = importLocalDockerImageForOverride
	dockerCLIAvailable                  = func() error {
		if _, err := exec.LookPath("docker"); err != nil {
			return fmt.Errorf("docker CLI not found in PATH: %w", err)
		}
		return nil
	}
	dockerInspectImage = func(ctx context.Context, source string) (string, error) {
		inspectCmd := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", source)
		var inspectStdout bytes.Buffer
		var inspectStderr bytes.Buffer
		inspectCmd.Stdout = &inspectStdout
		inspectCmd.Stderr = &inspectStderr
		if err := inspectCmd.Run(); err != nil {
			errText := strings.TrimSpace(inspectStderr.String())
			if errText == "" {
				errText = err.Error()
			}
			return "", fmt.Errorf("inspect local docker image %q: %s", source, errText)
		}
		return strings.TrimSpace(inspectStdout.String()), nil
	}
	dockerCreateContainer = func(ctx context.Context, source string) (string, error) {
		createCmd := exec.CommandContext(ctx, "docker", "create", source)
		var createStdout bytes.Buffer
		var createStderr bytes.Buffer
		createCmd.Stdout = &createStdout
		createCmd.Stderr = &createStderr
		if err := createCmd.Run(); err != nil {
			errText := strings.TrimSpace(createStderr.String())
			if errText == "" {
				errText = err.Error()
			}
			return "", fmt.Errorf("create container from %q: %s", source, errText)
		}
		return strings.TrimSpace(createStdout.String()), nil
	}
	dockerExportContainer = func(ctx context.Context, containerID, source string, dst io.Writer) error {
		exportCmd := exec.CommandContext(ctx, "docker", "export", containerID)
		var exportStderr bytes.Buffer
		exportCmd.Stdout = dst
		exportCmd.Stderr = &exportStderr
		if err := exportCmd.Run(); err != nil {
			errText := strings.TrimSpace(exportStderr.String())
			if errText == "" {
				errText = err.Error()
			}
			return fmt.Errorf("export container %q from %q: %s", containerID, source, errText)
		}
		return nil
	}
	dockerRemoveContainer = func(containerID string) error {
		return exec.Command("docker", "rm", "-f", containerID).Run()
	}
	resolveReferenceForImageOverride = func(ctx context.Context, source string, allowLocal bool) (string, error) {
		if !allowLocal {
			return resolveReferenceForPolicyUpdate(ctx, source)
		}

		localRef, localErr := importLocalDockerImageForOverrideFn(ctx, source)
		if localErr == nil {
			return localRef, nil
		}

		resolved, err := resolveReferenceForPolicyUpdate(ctx, source)
		if err == nil {
			if allowLocal {
				resolvedRef, parseErr := name.ParseReference(resolved, name.WeakValidation)
				if parseErr != nil {
					return "", fmt.Errorf("resolve image digest for %q: %w", source, parseErr)
				}
				imageOS, imageArch, platformErr := resolveReferencePlatformConfig(ctx, resolvedRef)
				if platformErr != nil {
					return "", fmt.Errorf("resolve image digest for %q: %w", source, platformErr)
				}
				if err := imagemgr.ValidateImagePlatformForHost(imageOS, imageArch, runtime.GOARCH); err != nil {
					return "", fmt.Errorf("resolve image digest for %q: %w", source, err)
				}
			}
			return resolved, nil
		}
		if isExplicitRegistryReference(source) {
			return "", err
		}

		return "", fmt.Errorf("%w; local docker resolution failed: %v", err, localErr)
	}
)

func isExplicitRegistryReference(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	if parsed, err := ociref.ParseDigestReference(trimmed); err == nil {
		return isRegistryHostPrefix(parsed.Repository)
	}
	namePart := trimmed
	if at := strings.Index(namePart, "@"); at >= 0 {
		namePart = namePart[:at]
	}
	if colon := strings.LastIndex(namePart, ":"); colon > strings.LastIndex(namePart, "/") {
		namePart = namePart[:colon]
	}
	first := strings.TrimSpace(strings.SplitN(namePart, "/", 2)[0])
	return isRegistryHostPrefix(first)
}

func isRegistryHostPrefix(component string) bool {
	component = strings.TrimSpace(strings.ToLower(component))
	if component == "" {
		return false
	}
	if component == "localhost" {
		return true
	}
	return strings.Contains(component, ".") || strings.Contains(component, ":")
}

func isLocalControlPlaneEndpoint(host string) (bool, error) {
	ep, err := endpoint.Resolve(host)
	if err != nil {
		return false, err
	}
	return ep.Scheme == "unix", nil
}

func overrideCompiledPolicyImage(compiled *policy.CompiledPolicy, imageRefOverride string, allowLocal bool) (*policy.CompiledPolicy, error) {
	imageRefOverride = strings.TrimSpace(imageRefOverride)
	if imageRefOverride == "" {
		return compiled, nil
	}

	resolvedRef, err := resolveReferenceForImageOverride(context.Background(), imageRefOverride, allowLocal)
	if err != nil {
		return nil, fmt.Errorf("invalid --image value: %w", err)
	}
	parsedRef, err := ociref.ParseDigestReference(resolvedRef)
	if err != nil {
		return nil, fmt.Errorf("invalid --image value: %w", err)
	}

	pb := compiled.ToProto()
	pb.ImageRef = parsedRef.Original
	pb.ImageDigest = parsedRef.Digest()
	pb.Hash = ""

	overridden, err := policy.FromProto(pb)
	if err != nil {
		return nil, fmt.Errorf("apply --image override: %w", err)
	}
	return overridden, nil
}

func importLocalDockerImageForOverride(ctx context.Context, source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("image reference cannot be empty")
	}
	if err := dockerCLIAvailable(); err != nil {
		return "", err
	}

	imageID, err := dockerInspectImage(ctx, source)
	if err != nil {
		return "", err
	}
	if imageID == "" {
		return "", fmt.Errorf("inspect local docker image %q returned empty image id", source)
	}
	digestRef, err := ociref.ParseDigestReference("local/docker-image@" + imageID)
	if err != nil {
		return "", fmt.Errorf("inspect local docker image %q returned invalid image id %q: %w", source, imageID, err)
	}

	mgr, err := newImageManager()
	if err != nil {
		return "", err
	}

	cachedRecords, err := mgr.List(ctx)
	if err != nil {
		return "", err
	}
	for _, record := range cachedRecords {
		if record.Digest != digestRef.Digest() {
			continue
		}
		if _, statErr := os.Stat(record.RootFSPath); statErr == nil {
			_, _ = fmt.Fprintln(os.Stderr, renderActionLine("using", "cached local image override "+digestRef.Original, defaultTerminalPalette().info, shouldUseANSI(os.Stderr)))
			return digestRef.Original, nil
		}
	}

	_, _ = fmt.Fprintln(os.Stderr, renderActionLine("importing", fmt.Sprintf("local docker image %q into cleanroom cache (first run can take a while)", source), defaultTerminalPalette().info, shouldUseANSI(os.Stderr)))

	containerID, err := dockerCreateContainer(ctx, source)
	if err != nil {
		return "", err
	}
	if containerID == "" {
		return "", fmt.Errorf("create container from %q returned empty container id", source)
	}
	defer func() {
		_ = dockerRemoveContainer(containerID)
	}()

	exportFile, err := os.CreateTemp("", "cleanroom-local-image-*.tar")
	if err != nil {
		return "", fmt.Errorf("create temporary export file: %w", err)
	}
	defer func() {
		_ = exportFile.Close()
		_ = os.Remove(exportFile.Name())
	}()

	if err := dockerExportContainer(ctx, containerID, source, exportFile); err != nil {
		return "", err
	}
	if _, err := exportFile.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind temporary export file: %w", err)
	}

	if _, err := mgr.Import(ctx, digestRef.Original, "-", exportFile); err != nil {
		return "", fmt.Errorf("import local docker image %q into cleanroom cache: %w", source, err)
	}

	_, _ = fmt.Fprintln(os.Stderr, renderActionLine("imported", "local docker image override as "+digestRef.Original, defaultTerminalPalette().info, shouldUseANSI(os.Stderr)))
	return digestRef.Original, nil
}

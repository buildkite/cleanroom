package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/imagemgr"
)

func stubDockerImportHelpers(
	t *testing.T,
	inspectImage func(context.Context, string) (string, error),
	createContainer func(context.Context, string) (string, error),
	exportContainer func(context.Context, string, string, io.Writer) error,
	removeContainer func(string) error,
) {
	t.Helper()

	prevCLIAvailable := dockerCLIAvailable
	prevInspectImage := dockerInspectImage
	prevCreateContainer := dockerCreateContainer
	prevExportContainer := dockerExportContainer
	prevRemoveContainer := dockerRemoveContainer

	dockerCLIAvailable = func() error { return nil }
	dockerInspectImage = inspectImage
	dockerCreateContainer = createContainer
	dockerExportContainer = exportContainer
	dockerRemoveContainer = removeContainer

	t.Cleanup(func() {
		dockerCLIAvailable = prevCLIAvailable
		dockerInspectImage = prevInspectImage
		dockerCreateContainer = prevCreateContainer
		dockerExportContainer = prevExportContainer
		dockerRemoveContainer = prevRemoveContainer
	})
}

func TestImportLocalDockerImageForOverrideImportsContainerFS(t *testing.T) {
	const (
		source      = "alpine:dev"
		imageID     = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
		containerID = "container-123"
		wantRef     = "local/docker-image@" + imageID
		exportedTar = "tar-stream"
	)

	mgr := &stubImageManager{
		importRecord: imagemgr.Record{
			Ref:        wantRef,
			Digest:     imageID,
			RootFSPath: "/cache/local.ext4",
			SizeBytes:  int64(len(exportedTar)),
		},
	}
	replaceImageManagerFactory(t, mgr, nil)

	var removedContainer string
	stubDockerImportHelpers(
		t,
		func(_ context.Context, gotSource string) (string, error) {
			if gotSource != source {
				t.Fatalf("unexpected source passed to inspect: got %q want %q", gotSource, source)
			}
			return imageID, nil
		},
		func(_ context.Context, gotSource string) (string, error) {
			if gotSource != source {
				t.Fatalf("unexpected source passed to create: got %q want %q", gotSource, source)
			}
			return containerID, nil
		},
		func(_ context.Context, gotContainerID, gotSource string, dst io.Writer) error {
			if gotContainerID != containerID {
				t.Fatalf("unexpected container passed to export: got %q want %q", gotContainerID, containerID)
			}
			if gotSource != source {
				t.Fatalf("unexpected source passed to export: got %q want %q", gotSource, source)
			}
			_, err := io.WriteString(dst, exportedTar)
			return err
		},
		func(gotContainerID string) error {
			removedContainer = gotContainerID
			return nil
		},
	)

	var resolvedRef string
	outcome := runWithCapture(func(*runtimeContext) error {
		ref, err := importLocalDockerImageForOverride(context.Background(), source)
		resolvedRef = ref
		return err
	}, nil, runtimeContext{})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("importLocalDockerImageForOverride returned error: %v", outcome.err)
	}
	if got := resolvedRef; got != wantRef {
		t.Fatalf("unexpected resolved ref: got %q want %q", got, wantRef)
	}
	if got := mgr.importRef; got != wantRef {
		t.Fatalf("unexpected ref passed to Import: got %q want %q", got, wantRef)
	}
	if got := mgr.importTarPath; got != "-" {
		t.Fatalf("unexpected tar path passed to Import: got %q want %q", got, "-")
	}
	if got := mgr.importStdin; got != exportedTar {
		t.Fatalf("unexpected exported tar passed to Import: got %q want %q", got, exportedTar)
	}
	if got := removedContainer; got != containerID {
		t.Fatalf("expected deferred docker rm for %q, got %q", containerID, got)
	}
	assertContainsAll(t, outcome.stderr,
		"importing local docker image \""+source+"\" into cleanroom cache",
		"imported local docker image override as "+wantRef,
	)
}

func TestImportLocalDockerImageForOverrideUsesANSIWhenForced(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")

	const (
		source      = "alpine:dev"
		imageID     = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
		containerID = "container-456"
		wantRef     = "local/docker-image@" + imageID
		exportedTar = "tar-stream"
	)

	mgr := &stubImageManager{
		importRecord: imagemgr.Record{
			Ref:        wantRef,
			Digest:     imageID,
			RootFSPath: "/cache/local.ext4",
			SizeBytes:  int64(len(exportedTar)),
		},
	}
	replaceImageManagerFactory(t, mgr, nil)

	stubDockerImportHelpers(
		t,
		func(_ context.Context, gotSource string) (string, error) {
			if gotSource != source {
				t.Fatalf("unexpected source passed to inspect: got %q want %q", gotSource, source)
			}
			return imageID, nil
		},
		func(_ context.Context, gotSource string) (string, error) {
			if gotSource != source {
				t.Fatalf("unexpected source passed to create: got %q want %q", gotSource, source)
			}
			return containerID, nil
		},
		func(_ context.Context, gotContainerID, gotSource string, dst io.Writer) error {
			if gotContainerID != containerID {
				t.Fatalf("unexpected container passed to export: got %q want %q", gotContainerID, containerID)
			}
			if gotSource != source {
				t.Fatalf("unexpected source passed to export: got %q want %q", gotSource, source)
			}
			_, err := io.WriteString(dst, exportedTar)
			return err
		},
		func(string) error { return nil },
	)

	outcome := runWithCapture(func(*runtimeContext) error {
		_, err := importLocalDockerImageForOverride(context.Background(), source)
		return err
	}, nil, runtimeContext{})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("importLocalDockerImageForOverride returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stderr, "\x1b[") {
		t.Fatalf("expected ANSI escapes in color output: %q", outcome.stderr)
	}
	assertContainsAll(t, stripANSI(outcome.stderr),
		"importing local docker image \""+source+"\" into cleanroom cache",
		"imported local docker image override as "+wantRef,
	)
}

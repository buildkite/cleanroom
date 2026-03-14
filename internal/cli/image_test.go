package cli

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/imagemgr"
)

type stubImageManager struct {
	pullRef    string
	pullResult imagemgr.EnsureResult
	pullErr    error

	listResult []imagemgr.Record
	listErr    error

	removeSelector string
	removeResult   []imagemgr.Record
	removeErr      error

	importRef     string
	importTarPath string
	importStdin   string
	importRecord  imagemgr.Record
	importErr     error
}

func (s *stubImageManager) Pull(_ context.Context, ref string) (imagemgr.EnsureResult, error) {
	s.pullRef = ref
	return s.pullResult, s.pullErr
}

func (s *stubImageManager) List(_ context.Context) ([]imagemgr.Record, error) {
	return s.listResult, s.listErr
}

func (s *stubImageManager) Remove(_ context.Context, selector string) ([]imagemgr.Record, error) {
	s.removeSelector = selector
	return s.removeResult, s.removeErr
}

func (s *stubImageManager) Import(_ context.Context, ref, tarPath string, stdin io.Reader) (imagemgr.Record, error) {
	s.importRef = ref
	s.importTarPath = tarPath
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return imagemgr.Record{}, err
		}
		s.importStdin = string(b)
	}
	return s.importRecord, s.importErr
}

func replaceImageManagerFactory(t *testing.T, mgr imageManager, err error) {
	t.Helper()

	prev := newImageManager
	newImageManager = func() (imageManager, error) {
		return mgr, err
	}
	t.Cleanup(func() {
		newImageManager = prev
	})
}

func TestImagePullCommandRunPrintsResult(t *testing.T) {
	mgr := &stubImageManager{
		pullResult: imagemgr.EnsureResult{
			Record: imagemgr.Record{
				Ref:        "ghcr.io/buildkite/cleanroom-base/alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Digest:     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				RootFSPath: "/tmp/rootfs.ext4",
				SizeBytes:  1234,
			},
		},
	}
	replaceImageManagerFactory(t, mgr, nil)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&ImagePullCommand{Ref: mgr.pullResult.Record.Ref}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("ImagePullCommand.Run returned error: %v", err)
	}
	if got, want := mgr.pullRef, mgr.pullResult.Record.Ref; got != want {
		t.Fatalf("unexpected ref passed to Pull: got %q want %q", got, want)
	}

	output := readStdout()
	assertContainsAll(t, output,
		"pulled image",
		"ref="+mgr.pullResult.Record.Ref,
		"digest="+mgr.pullResult.Record.Digest,
		"rootfs="+mgr.pullResult.Record.RootFSPath,
		"size_bytes=1234",
	)
}

func TestImageListCommandRunPrintsTable(t *testing.T) {
	lastUsed := time.Date(2026, time.March, 14, 9, 30, 0, 0, time.UTC)
	mgr := &stubImageManager{
		listResult: []imagemgr.Record{
			{
				Digest:     "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				Ref:        "ghcr.io/buildkite/cleanroom-base/alpine@sha256:2222222222222222222222222222222222222222222222222222222222222222",
				RootFSPath: "/cache/rootfs.ext4",
				SizeBytes:  2048,
				LastUsedAt: lastUsed,
			},
		},
	}
	replaceImageManagerFactory(t, mgr, nil)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&ImageListCommand{}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("ImageListCommand.Run returned error: %v", err)
	}

	output := readStdout()
	assertContainsAll(t, output,
		"DIGEST",
		"REF",
		"LAST_USED",
		mgr.listResult[0].Digest,
		mgr.listResult[0].Ref,
		mgr.listResult[0].RootFSPath,
		lastUsed.Format(time.RFC3339),
	)
}

func TestImageListCommandRunJSON(t *testing.T) {
	mgr := &stubImageManager{
		listResult: []imagemgr.Record{
			{
				Digest:     "sha256:3333333333333333333333333333333333333333333333333333333333333333",
				Ref:        "ghcr.io/buildkite/cleanroom-base/alpine@sha256:3333333333333333333333333333333333333333333333333333333333333333",
				RootFSPath: "/cache/rootfs.ext4",
				SizeBytes:  4096,
			},
		},
	}
	replaceImageManagerFactory(t, mgr, nil)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&ImageListCommand{JSON: true}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("ImageListCommand.Run returned error: %v", err)
	}

	var payload []imagemgr.Record
	if err := json.Unmarshal([]byte(readStdout()), &payload); err != nil {
		t.Fatalf("unmarshal JSON output: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("expected one image record, got %d", len(payload))
	}
	if got, want := payload[0].Digest, mgr.listResult[0].Digest; got != want {
		t.Fatalf("unexpected Digest in JSON output: got %q want %q", got, want)
	}
	if got, want := payload[0].Ref, mgr.listResult[0].Ref; got != want {
		t.Fatalf("unexpected Ref in JSON output: got %q want %q", got, want)
	}
}

func TestImageRemoveCommandRunPrintsRemovedItems(t *testing.T) {
	mgr := &stubImageManager{
		removeResult: []imagemgr.Record{
			{
				Digest: "sha256:4444444444444444444444444444444444444444444444444444444444444444",
				Ref:    "ghcr.io/buildkite/cleanroom-base/alpine@sha256:4444444444444444444444444444444444444444444444444444444444444444",
			},
		},
	}
	replaceImageManagerFactory(t, mgr, nil)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&ImageRemoveCommand{Selector: "sha256:4444"}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("ImageRemoveCommand.Run returned error: %v", err)
	}
	if got, want := mgr.removeSelector, "sha256:4444"; got != want {
		t.Fatalf("unexpected selector passed to Remove: got %q want %q", got, want)
	}

	output := readStdout()
	assertContainsAll(t, output, "removed "+mgr.removeResult[0].Digest+" ("+mgr.removeResult[0].Ref+")")
}

func TestImageImportCommandRunImportsFromStdin(t *testing.T) {
	mgr := &stubImageManager{
		importRecord: imagemgr.Record{
			Ref:        "ghcr.io/buildkite/cleanroom-base/alpine@sha256:5555555555555555555555555555555555555555555555555555555555555555",
			Digest:     "sha256:5555555555555555555555555555555555555555555555555555555555555555",
			RootFSPath: "/cache/imported.ext4",
			SizeBytes:  12,
		},
	}
	replaceImageManagerFactory(t, mgr, nil)

	stdinData := "tar-stream"
	outcome := runWithCapture(func(runCtx *runtimeContext) error {
		return (&ImageImportCommand{
			Ref:     mgr.importRecord.Ref,
			TarPath: "-",
		}).Run(runCtx)
	}, &stdinData, runtimeContext{})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ImageImportCommand.Run returned error: %v", outcome.err)
	}
	if got, want := mgr.importRef, mgr.importRecord.Ref; got != want {
		t.Fatalf("unexpected ref passed to Import: got %q want %q", got, want)
	}
	if got, want := mgr.importTarPath, "-"; got != want {
		t.Fatalf("unexpected tar path passed to Import: got %q want %q", got, want)
	}
	if got, want := mgr.importStdin, stdinData; got != want {
		t.Fatalf("unexpected stdin passed to Import: got %q want %q", got, want)
	}

	assertContainsAll(t, outcome.stdout,
		"imported image",
		"ref="+mgr.importRecord.Ref,
		"digest="+mgr.importRecord.Digest,
		"rootfs="+mgr.importRecord.RootFSPath,
		"size_bytes=12",
	)
}

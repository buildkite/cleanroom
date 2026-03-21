package paths

import (
	"path/filepath"
	"testing"
)

func TestBaseDirsPreferExplicitXDGHomes(t *testing.T) {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "/xdg/cache")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")

	cacheDir, err := CacheBaseDir()
	if err != nil {
		t.Fatalf("CacheBaseDir returned error: %v", err)
	}
	if got, want := cacheDir, "/xdg/cache/cleanroom"; got != want {
		t.Fatalf("unexpected cache dir: got %q want %q", got, want)
	}

	stateDir, err := StateBaseDir()
	if err != nil {
		t.Fatalf("StateBaseDir returned error: %v", err)
	}
	if got, want := stateDir, "/xdg/state/cleanroom"; got != want {
		t.Fatalf("unexpected state dir: got %q want %q", got, want)
	}

	dataDir, err := DataBaseDir()
	if err != nil {
		t.Fatalf("DataBaseDir returned error: %v", err)
	}
	if got, want := dataDir, "/xdg/data/cleanroom"; got != want {
		t.Fatalf("unexpected data dir: got %q want %q", got, want)
	}

	tlsDir, err := TLSDir()
	if err != nil {
		t.Fatalf("TLSDir returned error: %v", err)
	}
	if got, want := tlsDir, "/xdg/config/cleanroom/tls"; got != want {
		t.Fatalf("unexpected tls dir: got %q want %q", got, want)
	}
}

func TestBaseDirsFallBackToHomeDirectories(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	cacheDir, err := CacheBaseDir()
	if err != nil {
		t.Fatalf("CacheBaseDir returned error: %v", err)
	}
	if got, want := cacheDir, filepath.Join(home, ".cache", "cleanroom"); got != want {
		t.Fatalf("unexpected cache dir: got %q want %q", got, want)
	}

	stateDir, err := StateBaseDir()
	if err != nil {
		t.Fatalf("StateBaseDir returned error: %v", err)
	}
	if got, want := stateDir, filepath.Join(home, ".local", "state", "cleanroom"); got != want {
		t.Fatalf("unexpected state dir: got %q want %q", got, want)
	}

	dataDir, err := DataBaseDir()
	if err != nil {
		t.Fatalf("DataBaseDir returned error: %v", err)
	}
	if got, want := dataDir, filepath.Join(home, ".local", "share", "cleanroom"); got != want {
		t.Fatalf("unexpected data dir: got %q want %q", got, want)
	}

	tlsDir, err := TLSDir()
	if err != nil {
		t.Fatalf("TLSDir returned error: %v", err)
	}
	if got, want := tlsDir, filepath.Join(home, ".config", "cleanroom", "tls"); got != want {
		t.Fatalf("unexpected tls dir: got %q want %q", got, want)
	}
}

func TestDerivedPathsUseResolvedBaseDirectories(t *testing.T) {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "/xdg/cache")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")

	imageCacheDir, err := ImageCacheDir()
	if err != nil {
		t.Fatalf("ImageCacheDir returned error: %v", err)
	}
	if got, want := imageCacheDir, "/xdg/cache/cleanroom/images"; got != want {
		t.Fatalf("unexpected image cache dir: got %q want %q", got, want)
	}

	imageMetadataDBPath, err := ImageMetadataDBPath()
	if err != nil {
		t.Fatalf("ImageMetadataDBPath returned error: %v", err)
	}
	if got, want := imageMetadataDBPath, "/xdg/state/cleanroom/images/metadata.db"; got != want {
		t.Fatalf("unexpected image metadata db path: got %q want %q", got, want)
	}

	executionBaseDir, err := ExecutionBaseDir()
	if err != nil {
		t.Fatalf("ExecutionBaseDir returned error: %v", err)
	}
	if got, want := executionBaseDir, "/xdg/state/cleanroom/executions"; got != want {
		t.Fatalf("unexpected execution base dir: got %q want %q", got, want)
	}

	snapshotDir, err := SnapshotDir()
	if err != nil {
		t.Fatalf("SnapshotDir returned error: %v", err)
	}
	if got, want := snapshotDir, "/xdg/state/cleanroom/snapshots"; got != want {
		t.Fatalf("unexpected snapshot dir: got %q want %q", got, want)
	}

	snapshotMetadataDBPath, err := SnapshotMetadataDBPath()
	if err != nil {
		t.Fatalf("SnapshotMetadataDBPath returned error: %v", err)
	}
	if got, want := snapshotMetadataDBPath, "/xdg/state/cleanroom/snapshots/metadata.db"; got != want {
		t.Fatalf("unexpected snapshot metadata db path: got %q want %q", got, want)
	}

	assetsDir, err := AssetsDir()
	if err != nil {
		t.Fatalf("AssetsDir returned error: %v", err)
	}
	if got, want := assetsDir, "/xdg/data/cleanroom/assets"; got != want {
		t.Fatalf("unexpected assets dir: got %q want %q", got, want)
	}
}

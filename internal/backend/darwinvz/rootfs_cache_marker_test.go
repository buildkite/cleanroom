//go:build darwin

package darwinvz

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPreparedRuntimeRootFSCacheHitUsesValidMarkerWithoutExt4Validation(t *testing.T) {
	rootFSPath := writeTestPreparedRootFS(t, "prepared-rootfs")
	if err := writePreparedRuntimeRootFSMarker(rootFSPath); err != nil {
		t.Fatalf("write prepared rootfs marker: %v", err)
	}

	validateCalls := 0
	restore := stubPreparedRuntimeRootFSValidator(t, func(string) error {
		validateCalls++
		return errors.New("validator should not be called")
	})
	defer restore()

	if !preparedRuntimeRootFSCacheHitIsValid(rootFSPath) {
		t.Fatal("expected prepared rootfs cache hit to be valid")
	}
	if validateCalls != 0 {
		t.Fatalf("expected validation to be skipped, got %d calls", validateCalls)
	}
}

func TestPreparedRuntimeRootFSCacheHitValidatesAndMarksWhenMarkerMissing(t *testing.T) {
	rootFSPath := writeTestPreparedRootFS(t, "prepared-rootfs")

	validateCalls := 0
	restore := stubPreparedRuntimeRootFSValidator(t, func(string) error {
		validateCalls++
		return nil
	})
	defer restore()

	if !preparedRuntimeRootFSCacheHitIsValid(rootFSPath) {
		t.Fatal("expected prepared rootfs cache hit to be valid after validation")
	}
	if validateCalls != 1 {
		t.Fatalf("expected one validation call, got %d", validateCalls)
	}
	if _, err := os.Stat(preparedRuntimeRootFSMarkerPath(rootFSPath)); err != nil {
		t.Fatalf("expected prepared rootfs marker to be written: %v", err)
	}

	if !preparedRuntimeRootFSCacheHitIsValid(rootFSPath) {
		t.Fatal("expected marked prepared rootfs cache hit to remain valid")
	}
	if validateCalls != 1 {
		t.Fatalf("expected marker to skip second validation, got %d calls", validateCalls)
	}
}

func TestPreparedRuntimeRootFSCacheHitRevalidatesStaleMarker(t *testing.T) {
	rootFSPath := writeTestPreparedRootFS(t, "prepared-rootfs")
	if err := writePreparedRuntimeRootFSMarker(rootFSPath); err != nil {
		t.Fatalf("write prepared rootfs marker: %v", err)
	}
	if err := os.WriteFile(rootFSPath, []byte("prepared-rootfs-mutated"), 0o644); err != nil {
		t.Fatalf("mutate prepared rootfs: %v", err)
	}

	validateCalls := 0
	restore := stubPreparedRuntimeRootFSValidator(t, func(string) error {
		validateCalls++
		return nil
	})
	defer restore()

	if !preparedRuntimeRootFSCacheHitIsValid(rootFSPath) {
		t.Fatal("expected stale marker to be refreshed after validation")
	}
	if validateCalls != 1 {
		t.Fatalf("expected stale marker to force validation once, got %d calls", validateCalls)
	}
	if !preparedRuntimeRootFSMarkerMatches(rootFSPath) {
		t.Fatal("expected refreshed marker to match mutated rootfs")
	}
}

func TestPreparedRuntimeRootFSCacheHitRevalidatesWhenContentsChangeWithPreservedSizeAndModTime(t *testing.T) {
	rootFSPath := writeTestPreparedRootFS(t, "prepared-rootfs")
	info, err := os.Stat(rootFSPath)
	if err != nil {
		t.Fatalf("stat prepared rootfs: %v", err)
	}
	if err := writePreparedRuntimeRootFSMarker(rootFSPath); err != nil {
		t.Fatalf("write prepared rootfs marker: %v", err)
	}
	if err := os.WriteFile(rootFSPath, []byte("corrupted-rootf"), 0o644); err != nil {
		t.Fatalf("mutate prepared rootfs: %v", err)
	}
	if err := os.Chtimes(rootFSPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore prepared rootfs mtime: %v", err)
	}

	validateCalls := 0
	restore := stubPreparedRuntimeRootFSValidator(t, func(string) error {
		validateCalls++
		return nil
	})
	defer restore()

	if !preparedRuntimeRootFSCacheHitIsValid(rootFSPath) {
		t.Fatal("expected marker with changed contents to be refreshed after validation")
	}
	if validateCalls != 1 {
		t.Fatalf("expected content change with preserved size and mtime to force validation once, got %d calls", validateCalls)
	}
	if !preparedRuntimeRootFSMarkerMatches(rootFSPath) {
		t.Fatal("expected refreshed marker to match mutated rootfs")
	}
}

func TestPreparedRuntimeRootFSMarkerRejectsSubsecondOnlyTimestampMatch(t *testing.T) {
	rootFSPath := writeTestPreparedRootFS(t, "prepared-rootfs")
	const nsec = int64(123456789)
	oldModTime := time.Unix(1_700_000_000, nsec)
	newModTime := time.Unix(1_700_000_001, nsec)
	if err := os.Chtimes(rootFSPath, oldModTime, oldModTime); err != nil {
		t.Fatalf("set initial prepared rootfs timestamps: %v", err)
	}
	if err := os.Chtimes(rootFSPath, newModTime, newModTime); err != nil {
		t.Fatalf("preserve prepared rootfs nsec while changing seconds: %v", err)
	}

	state, err := preparedRuntimeRootFSMarkerStateForPath(rootFSPath)
	if err != nil {
		t.Fatalf("stat prepared rootfs marker state: %v", err)
	}
	oldStyleContent := fmt.Sprintf("%s\n%d\n%d\n%d\n",
		preparedRuntimeRootFSMarkerVersion,
		state.size,
		nsec,
		state.changeTimeNanos%int64(time.Second),
	)
	if err := os.WriteFile(preparedRuntimeRootFSMarkerPath(rootFSPath), []byte(oldStyleContent), 0o644); err != nil {
		t.Fatalf("write old-style prepared rootfs marker: %v", err)
	}

	if preparedRuntimeRootFSMarkerMatches(rootFSPath) {
		t.Fatal("expected subsecond-only marker to be stale after timestamp seconds changed")
	}
}

func TestPreparedRuntimeRootFSCacheHitRejectsInvalidPreparedRootFS(t *testing.T) {
	rootFSPath := writeTestPreparedRootFS(t, "prepared-rootfs")

	validateCalls := 0
	restore := stubPreparedRuntimeRootFSValidator(t, func(string) error {
		validateCalls++
		return errors.New("missing guest runtime")
	})
	defer restore()

	if preparedRuntimeRootFSCacheHitIsValid(rootFSPath) {
		t.Fatal("expected invalid prepared rootfs cache hit to be rejected")
	}
	if validateCalls != 1 {
		t.Fatalf("expected one validation call, got %d", validateCalls)
	}
	if _, err := os.Stat(preparedRuntimeRootFSMarkerPath(rootFSPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected marker not to be written, got err=%v", err)
	}
}

func writeTestPreparedRootFS(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write prepared rootfs: %v", err)
	}
	return path
}

func stubPreparedRuntimeRootFSValidator(t *testing.T, fn func(string) error) func() {
	t.Helper()

	prev := validatePreparedRuntimeRootFSFn
	validatePreparedRuntimeRootFSFn = fn
	return func() {
		validatePreparedRuntimeRootFSFn = prev
	}
}

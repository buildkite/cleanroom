//go:build darwin

package darwinvz

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVirtualizationProcessPIDLookupRunsAsynchronously(t *testing.T) {
	restoreNetworkProcessLookupGlobals(t)

	var startedOnce sync.Once
	lsofStarted := make(chan struct{})
	releaseLsof := make(chan struct{})
	networkProcessLookupTimeout = time.Second
	networkProcessLookPath = func(name string) (string, error) {
		return "/test/bin/" + name, nil
	}
	networkProcessCombinedOutput = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case strings.HasSuffix(name, "/lsof"):
			startedOnce.Do(func() { close(lsofStarted) })
			select {
			case <-releaseLsof:
				return []byte("4321\n"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case strings.HasSuffix(name, "/ps"):
			return []byte(virtualizationNetworkProcessPath + "\n"), nil
		default:
			t.Fatalf("unexpected command %q args=%v", name, args)
			return nil, nil
		}
	}

	lookup := startVirtualizationProcessPIDLookup(context.Background(), "/tmp/rootfs.ext4")
	defer lookup.stop()

	select {
	case <-lsofStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async lsof lookup to start")
	}

	select {
	case result := <-lookup.resultCh:
		t.Fatalf("pid lookup finished before lsof was released: %#v", result)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseLsof)
	result := lookup.wait()
	if result.err != nil {
		t.Fatalf("pid lookup returned error: %v", result.err)
	}
	if got, want := result.pid, 4321; got != want {
		t.Fatalf("unexpected pid: got %d want %d", got, want)
	}
	if result.duration < 25*time.Millisecond {
		t.Fatalf("lookup duration was not measured across the blocked lsof call: %s", result.duration)
	}
}

func TestVirtualizationProcessPIDLookupStopCancelsLookup(t *testing.T) {
	restoreNetworkProcessLookupGlobals(t)

	var startedOnce sync.Once
	lsofStarted := make(chan struct{})
	networkProcessLookupTimeout = time.Second
	networkProcessLookPath = func(name string) (string, error) {
		return "/test/bin/" + name, nil
	}
	networkProcessCombinedOutput = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if !strings.HasSuffix(name, "/lsof") {
			t.Fatalf("unexpected command %q args=%v", name, args)
		}
		startedOnce.Do(func() { close(lsofStarted) })
		<-ctx.Done()
		return nil, ctx.Err()
	}

	lookup := startVirtualizationProcessPIDLookup(context.Background(), "/tmp/rootfs.ext4")
	select {
	case <-lsofStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async lsof lookup to start")
	}
	lookup.stop()

	select {
	case result := <-lookup.resultCh:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("expected canceled lookup, got %v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled lookup to finish")
	}
}

func restoreNetworkProcessLookupGlobals(t *testing.T) {
	t.Helper()

	origLookPath := networkProcessLookPath
	origCombinedOutput := networkProcessCombinedOutput
	origTimeout := networkProcessLookupTimeout
	origPollInterval := networkProcessLookupPollInterval
	t.Cleanup(func() {
		networkProcessLookPath = origLookPath
		networkProcessCombinedOutput = origCombinedOutput
		networkProcessLookupTimeout = origTimeout
		networkProcessLookupPollInterval = origPollInterval
	})
}

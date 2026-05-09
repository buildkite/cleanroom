package main

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestAppendHostsLineIfMissingPreservesExistingEntries(t *testing.T) {
	t.Parallel()

	content := []byte("10.0.0.2 buildkit.local\n127.0.0.1 localhost existing\n")
	got := appendHostsLineIfMissing(content, "127.0.0.1", "localhost", "127.0.0.1 localhost")

	if string(got) != string(content) {
		t.Fatalf("expected existing localhost entry to be preserved, got %q", got)
	}
}

func TestAppendHostsLineIfMissingAddsSeparatorNewline(t *testing.T) {
	t.Parallel()

	got := appendHostsLineIfMissing([]byte("10.0.0.2 buildkit.local"), "::1", "localhost", "::1 localhost ip6-localhost ip6-loopback")
	want := "10.0.0.2 buildkit.local\n::1 localhost ip6-localhost ip6-loopback\n"
	if string(got) != want {
		t.Fatalf("unexpected hosts content:\ngot  %q\nwant %q", got, want)
	}
}

func TestHostsHasAddressNameIgnoresComments(t *testing.T) {
	t.Parallel()

	if hostsHasAddressName("# 127.0.0.1 localhost\n", "127.0.0.1", "localhost") {
		t.Fatal("commented localhost entry should not match")
	}
	if !hostsHasAddressName("127.0.0.1 localhost # local loopback\n", "127.0.0.1", "localhost") {
		t.Fatal("expected localhost entry before comment to match")
	}
}

func TestPrefixToIPv4Mask(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		prefix int
		want   string
		ok     bool
	}{
		{prefix: 0, want: "0.0.0.0", ok: true},
		{prefix: 24, want: "255.255.255.0", ok: true},
		{prefix: 30, want: "255.255.255.252", ok: true},
		{prefix: 32, want: "255.255.255.255", ok: true},
		{prefix: -1, ok: false},
		{prefix: 33, ok: false},
	} {
		got, ok := prefixToIPv4Mask(tc.prefix)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("prefixToIPv4Mask(%d): got %q, %t want %q, %t", tc.prefix, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFirstNonLoopbackInterface(t *testing.T) {
	t.Parallel()

	got := firstNonLoopbackInterface([]string{"", " lo ", "enp0s1", "eth0"})
	if got != "enp0s1" {
		t.Fatalf("unexpected interface: %q", got)
	}
	if got := firstNonLoopbackInterface([]string{"lo", " "}); got != "" {
		t.Fatalf("expected no non-loopback interface, got %q", got)
	}
}

func TestFirstCharacterDevice(t *testing.T) {
	t.Parallel()

	got := firstCharacterDevice([]string{"/missing", "/regular", "/char"}, func(path string) (os.FileInfo, error) {
		switch path {
		case "/regular":
			return fakeInitFileInfo{mode: 0o644}, nil
		case "/char":
			return fakeInitFileInfo{mode: os.ModeCharDevice | 0o600}, nil
		default:
			return nil, errors.New("missing")
		}
	})

	if got != "/char" {
		t.Fatalf("unexpected character device path: %q", got)
	}
}

type fakeInitFileInfo struct {
	mode os.FileMode
}

func (f fakeInitFileInfo) Name() string       { return "" }
func (f fakeInitFileInfo) Size() int64        { return 0 }
func (f fakeInitFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInitFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInitFileInfo) IsDir() bool        { return false }
func (f fakeInitFileInfo) Sys() any           { return nil }

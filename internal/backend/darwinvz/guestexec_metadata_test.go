//go:build darwin

package darwinvz

import "testing"

func TestDarwinVZAttachMetadataIncludesNetworkDetails(t *testing.T) {
	got := darwinVZAttachMetadata(1234, &darwinVZNetworkMetadata{GuestIP: " 192.0.2.10 "})
	if got == nil {
		t.Fatal("expected attach metadata")
	}
	if got["network_process_pid"] != "1234" {
		t.Fatalf("unexpected network_process_pid: %q", got["network_process_pid"])
	}
	if got["network_guest_ip"] != "192.0.2.10" {
		t.Fatalf("unexpected network_guest_ip: %q", got["network_guest_ip"])
	}
}

func TestDarwinVZAttachMetadataOmitsEmptyDetails(t *testing.T) {
	if got := darwinVZAttachMetadata(0, nil); got != nil {
		t.Fatalf("expected nil metadata, got %#v", got)
	}
	if got := darwinVZAttachMetadata(0, &darwinVZNetworkMetadata{GuestIP: " "}); got != nil {
		t.Fatalf("expected nil metadata for blank guest IP, got %#v", got)
	}
}

//go:build linux

package firecracker

import "testing"

func TestNFLogGroupFromTapNameDeterministic(t *testing.T) {
	t.Parallel()

	g1 := nflogGroupFromTapName("cr0-tap")
	g2 := nflogGroupFromTapName("cr0-tap")
	if g1 != g2 {
		t.Fatalf("same tap name returned different groups: %d vs %d", g1, g2)
	}

	if g1 < 100 || g1 > 65535 {
		t.Fatalf("group %d out of range [100, 65535]", g1)
	}

	g3 := nflogGroupFromTapName("cr1-tap")
	if g3 < 100 || g3 > 65535 {
		t.Fatalf("group %d out of range [100, 65535]", g3)
	}
}

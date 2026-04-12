package firecracker

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
)

func buildTestIPv4Packet(srcIP, dstIP net.IP, proto byte, srcPort, dstPort uint16) []byte {
	pkt := make([]byte, 24) // 20 byte IP header + 4 bytes transport (ports)
	pkt[0] = 0x45           // version 4, IHL 5
	pkt[9] = proto
	copy(pkt[12:16], srcIP.To4())
	copy(pkt[16:20], dstIP.To4())
	binary.BigEndian.PutUint16(pkt[20:22], srcPort)
	binary.BigEndian.PutUint16(pkt[22:24], dstPort)
	return pkt
}

func TestParseIPv4PacketHeaderTCP(t *testing.T) {
	t.Parallel()

	pkt := buildTestIPv4Packet(
		net.IPv4(10, 0, 0, 2),
		net.IPv4(203, 0, 113, 50),
		6, // TCP
		12345,
		443,
	)

	dstIP, dstPort, proto, ok := parseIPv4PacketHeader(pkt)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := netip.MustParseAddr("203.0.113.50"); dstIP != want {
		t.Errorf("dstIP = %v, want %v", dstIP, want)
	}
	if dstPort != 443 {
		t.Errorf("dstPort = %d, want 443", dstPort)
	}
	if proto != "tcp" {
		t.Errorf("proto = %q, want %q", proto, "tcp")
	}
}

func TestParseIPv4PacketHeaderUDP(t *testing.T) {
	t.Parallel()

	pkt := buildTestIPv4Packet(
		net.IPv4(10, 0, 0, 2),
		net.IPv4(203, 0, 113, 50),
		17, // UDP
		12345,
		53,
	)

	dstIP, dstPort, proto, ok := parseIPv4PacketHeader(pkt)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := netip.MustParseAddr("203.0.113.50"); dstIP != want {
		t.Errorf("dstIP = %v, want %v", dstIP, want)
	}
	if dstPort != 53 {
		t.Errorf("dstPort = %d, want 53", dstPort)
	}
	if proto != "udp" {
		t.Errorf("proto = %q, want %q", proto, "udp")
	}
}

func TestParseIPv4PacketHeaderTooShort(t *testing.T) {
	t.Parallel()

	pkt := make([]byte, 10)
	_, _, _, ok := parseIPv4PacketHeader(pkt)
	if ok {
		t.Fatal("expected ok=false for too-short packet")
	}
}

func TestParseIPv4PacketHeaderNotIPv4(t *testing.T) {
	t.Parallel()

	pkt := buildTestIPv4Packet(net.IPv4(10, 0, 0, 2), net.IPv4(203, 0, 113, 50), 6, 12345, 443)
	pkt[0] = 0x65 // version 6
	_, _, _, ok := parseIPv4PacketHeader(pkt)
	if ok {
		t.Fatal("expected ok=false for non-IPv4 packet")
	}
}

func TestParseIPv4PacketHeaderUnsupportedProtocol(t *testing.T) {
	t.Parallel()

	pkt := buildTestIPv4Packet(net.IPv4(10, 0, 0, 2), net.IPv4(203, 0, 113, 50), 1, 0, 0) // ICMP
	_, _, _, ok := parseIPv4PacketHeader(pkt)
	if ok {
		t.Fatal("expected ok=false for unsupported protocol")
	}
}

func TestParseIPv4PacketHeaderIPFragment(t *testing.T) {
	t.Parallel()

	pkt := buildTestIPv4Packet(net.IPv4(10, 0, 0, 2), net.IPv4(203, 0, 113, 50), 6, 12345, 443)
	// Set fragment offset to non-zero (bytes 6-7, lower 13 bits).
	binary.BigEndian.PutUint16(pkt[6:8], 0x0020) // offset 32
	_, _, _, ok := parseIPv4PacketHeader(pkt)
	if ok {
		t.Fatal("expected ok=false for non-initial IP fragment")
	}
}

func TestParseIPv4PacketHeaderTruncatedTransport(t *testing.T) {
	t.Parallel()

	pkt := buildTestIPv4Packet(net.IPv4(10, 0, 0, 2), net.IPv4(203, 0, 113, 50), 6, 12345, 443)
	pkt = pkt[:20] // valid IP header, no transport bytes
	_, _, _, ok := parseIPv4PacketHeader(pkt)
	if ok {
		t.Fatal("expected ok=false for truncated transport header")
	}
}

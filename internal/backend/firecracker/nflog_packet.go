package firecracker

import (
	"encoding/binary"
	"net/netip"
)

// parseIPv4PacketHeader extracts the destination IP, port, and protocol from
// a raw IPv4 packet. It returns ok=false if the packet is too short or uses
// a protocol other than TCP or UDP.
func parseIPv4PacketHeader(payload []byte) (destIP netip.Addr, destPort uint16, protocol string, ok bool) {
	if len(payload) < 20 {
		return destIP, 0, "", false
	}
	version := payload[0] >> 4
	if version != 4 {
		return destIP, 0, "", false
	}

	ihl := int(payload[0]&0x0f) * 4
	if ihl < 20 || len(payload) < ihl+4 {
		return destIP, 0, "", false
	}

	// Skip non-initial IP fragments — transport header is only in the first.
	fragOffset := binary.BigEndian.Uint16(payload[6:8]) & 0x1FFF
	if fragOffset != 0 {
		return destIP, 0, "", false
	}

	switch payload[9] {
	case 6:
		protocol = "tcp"
	case 17:
		protocol = "udp"
	default:
		return destIP, 0, "", false
	}

	destIP = netip.AddrFrom4([4]byte(payload[16:20]))
	destPort = binary.BigEndian.Uint16(payload[ihl+2 : ihl+4])
	return destIP, destPort, protocol, true
}

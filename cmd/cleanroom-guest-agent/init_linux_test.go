//go:build linux

package main

import (
	"bytes"
	"net"
	"os"
	"testing"

	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nlenc"
	"golang.org/x/sys/unix"
)

func TestMarshalIfAddrmsg(t *testing.T) {
	got := marshalIfAddrmsg(unix.IfAddrmsg{
		Family:    unix.AF_INET,
		Prefixlen: 8,
		Flags:     unix.IFA_F_PERMANENT,
		Scope:     unix.RT_SCOPE_HOST,
		Index:     42,
	})

	if len(got) != unix.SizeofIfAddrmsg {
		t.Fatalf("unexpected length: got %d want %d", len(got), unix.SizeofIfAddrmsg)
	}
	if got[0] != unix.AF_INET {
		t.Fatalf("unexpected family: got %d want %d", got[0], unix.AF_INET)
	}
	if got[1] != 8 {
		t.Fatalf("unexpected prefix length: got %d want 8", got[1])
	}
	if got[2] != unix.IFA_F_PERMANENT {
		t.Fatalf("unexpected flags: got %d want %d", got[2], unix.IFA_F_PERMANENT)
	}
	if got[3] != unix.RT_SCOPE_HOST {
		t.Fatalf("unexpected scope: got %d want %d", got[3], unix.RT_SCOPE_HOST)
	}
	if index := nlenc.Uint32(got[4:8]); index != 42 {
		t.Fatalf("unexpected index: got %d want 42", index)
	}
}

func TestInterfaceAddressMessageData(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1).To4()
	got := interfaceAddressMessageData(7, ip, 8, unix.AF_INET, unix.RT_SCOPE_HOST, 0)

	if len(got) <= unix.SizeofIfAddrmsg {
		t.Fatalf("expected address message attributes, got %d bytes", len(got))
	}
	if got[0] != unix.AF_INET {
		t.Fatalf("unexpected family: got %d want %d", got[0], unix.AF_INET)
	}
	if got[1] != 8 {
		t.Fatalf("unexpected prefix length: got %d want 8", got[1])
	}
	if got[3] != unix.RT_SCOPE_HOST {
		t.Fatalf("unexpected scope: got %d want %d", got[3], unix.RT_SCOPE_HOST)
	}
	if index := nlenc.Uint32(got[4:8]); index != 7 {
		t.Fatalf("unexpected index: got %d want 7", index)
	}

	attrs, err := netlink.UnmarshalAttributes(got[unix.SizeofIfAddrmsg:])
	if err != nil {
		t.Fatalf("unmarshal attributes: %v", err)
	}
	for _, attr := range attrs {
		switch attr.Type {
		case unix.IFA_LOCAL, unix.IFA_ADDRESS:
			if !bytes.Equal(attr.Data, ip) {
				t.Fatalf("unexpected address attribute %d: got %v want %v", attr.Type, attr.Data, ip)
			}
		}
	}
	if countAttribute(attrs, unix.IFA_LOCAL) != 1 {
		t.Fatalf("expected one IFA_LOCAL attribute, got %d", countAttribute(attrs, unix.IFA_LOCAL))
	}
	if countAttribute(attrs, unix.IFA_ADDRESS) != 1 {
		t.Fatalf("expected one IFA_ADDRESS attribute, got %d", countAttribute(attrs, unix.IFA_ADDRESS))
	}
}

func TestDefaultIPv4RouteMessageData(t *testing.T) {
	gateway := net.IPv4(10, 233, 0, 1).To4()
	got := defaultIPv4RouteMessageData(9, gateway)

	if len(got) <= unix.SizeofRtMsg {
		t.Fatalf("expected route message attributes, got %d bytes", len(got))
	}
	if got[0] != unix.AF_INET {
		t.Fatalf("unexpected family: got %d want %d", got[0], unix.AF_INET)
	}
	if got[1] != 0 {
		t.Fatalf("unexpected destination prefix length: got %d want 0", got[1])
	}
	if got[4] != unix.RT_TABLE_MAIN {
		t.Fatalf("unexpected route table: got %d want %d", got[4], unix.RT_TABLE_MAIN)
	}
	if got[5] != unix.RTPROT_BOOT {
		t.Fatalf("unexpected route protocol: got %d want %d", got[5], unix.RTPROT_BOOT)
	}
	if got[6] != unix.RT_SCOPE_UNIVERSE {
		t.Fatalf("unexpected route scope: got %d want %d", got[6], unix.RT_SCOPE_UNIVERSE)
	}
	if got[7] != unix.RTN_UNICAST {
		t.Fatalf("unexpected route type: got %d want %d", got[7], unix.RTN_UNICAST)
	}

	attrs, err := netlink.UnmarshalAttributes(got[unix.SizeofRtMsg:])
	if err != nil {
		t.Fatalf("unmarshal attributes: %v", err)
	}
	if gatewayAttr := findAttribute(attrs, unix.RTA_GATEWAY); gatewayAttr == nil {
		t.Fatal("expected RTA_GATEWAY attribute")
	} else if !bytes.Equal(gatewayAttr.Data, gateway) {
		t.Fatalf("unexpected gateway: got %v want %v", gatewayAttr.Data, gateway)
	}
	if oifAttr := findAttribute(attrs, unix.RTA_OIF); oifAttr == nil {
		t.Fatal("expected RTA_OIF attribute")
	} else if index := nlenc.Uint32(oifAttr.Data); index != 9 {
		t.Fatalf("unexpected output interface index: got %d want 9", index)
	}
}

func TestSetupLoopbackNativeIntegration(t *testing.T) {
	if os.Getenv("CLEANROOM_TEST_NATIVE_LOOPBACK") != "1" {
		t.Skip("set CLEANROOM_TEST_NATIVE_LOOPBACK=1 to exercise Linux netlink loopback setup")
	}
	if os.Geteuid() != 0 {
		t.Skip("native loopback setup requires root")
	}

	if !setupLoopbackNative() {
		t.Fatal("setupLoopbackNative returned false")
	}
	iface, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatalf("lookup loopback: %v", err)
	}
	if iface.Flags&net.FlagUp == 0 {
		t.Fatalf("loopback is not up: flags=%v", iface.Flags)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		t.Fatalf("lookup loopback addresses: %v", err)
	}
	if !hasInterfaceAddress(addrs, "127.0.0.1") {
		t.Fatalf("loopback addresses missing 127.0.0.1: %v", addrs)
	}
	if !hasInterfaceAddress(addrs, "::1") {
		t.Fatalf("loopback addresses missing ::1: %v", addrs)
	}
}

func hasInterfaceAddress(addrs []net.Addr, want string) bool {
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip.Equal(net.ParseIP(want)) {
			return true
		}
	}
	return false
}

func findAttribute(attrs []netlink.Attribute, attrType uint16) *netlink.Attribute {
	for i := range attrs {
		if attrs[i].Type == attrType {
			return &attrs[i]
		}
	}
	return nil
}

func countAttribute(attrs []netlink.Attribute, attrType uint16) int {
	count := 0
	for _, attr := range attrs {
		if attr.Type == attrType {
			count++
		}
	}
	return count
}

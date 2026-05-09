//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/guestenv"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nlenc"
	"golang.org/x/sys/unix"
)

const guestAgentInstalledPath = "/usr/local/bin/cleanroom-guest-agent"

func runGuestInit() {
	configureGuestInitEnvironment()
	setupGuestInitMounts()
	ensureLocalhostHosts("/etc/hosts")

	cmdline := readKernelCmdline()
	setupGuestNetwork(cmdline)
	startStdioGuestAgentIfPresent()
	enableGuestInitBootTimings(cmdline)

	for {
		if err := runGuestAgentServer(); err != nil {
			fmt.Fprintf(os.Stderr, "cleanroom guest agent: %v\n", err)
		}
		time.Sleep(time.Second)
	}
}

func configureGuestInitEnvironment() {
	_ = os.Setenv("HOME", guestenv.DefaultHome)
	_ = os.Setenv("PATH", guestenv.DefaultPath)
}

func setupGuestInitMounts() {
	mountGuestFS("proc", "/proc", "proc")
	mountGuestFS("sysfs", "/sys", "sysfs")
	mountGuestFS("devtmpfs", "/dev", "devtmpfs")
	for _, path := range []string{"/dev/pts", "/run", "/tmp"} {
		_ = os.MkdirAll(path, 0o755)
	}
	mountGuestFS("devpts", "/dev/pts", "devpts")
	mountGuestFS("tmpfs", "/run", "tmpfs")
	mountGuestFS("tmpfs", "/tmp", "tmpfs")
}

func mountGuestFS(source, target, fstype string) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return
	}
	if err := unix.Mount(source, target, fstype, 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		return
	}
}

func ensureLocalhostHosts(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	next := appendHostsLineIfMissing(content, "127.0.0.1", "localhost", "127.0.0.1 localhost")
	next = appendHostsLineIfMissing(next, "::1", "localhost", "::1 localhost ip6-localhost ip6-loopback")
	if string(next) == string(content) {
		return
	}
	_ = os.WriteFile(path, next, 0o644)
}

func setupGuestNetwork(cmdline string) {
	setupLoopback()

	iface := firstNonLoopbackInterface(listNetworkInterfaces())
	if iface == "" {
		return
	}
	if configureVMNetStaticNetwork(cmdline, iface) {
		return
	}

	if commandExists("ip") {
		runCommandQuiet("ip", "link", "set", iface, "up")
	} else if commandExists("ifconfig") {
		runCommandQuiet("ifconfig", iface, "up")
	}

	if commandExists("udhcpc") {
		runCommandQuiet("udhcpc", "-q", "-n", "-t", "3", "-T", "3", "-i", iface)
	} else if commandExists("dhclient") {
		runCommandQuiet("dhclient", "-1", iface)
	}
}

func setupLoopback() {
	if setupLoopbackNative() {
		return
	}

	if commandExists("ip") {
		runCommandQuiet("ip", "link", "set", "lo", "up")
		runCommandQuiet("ip", "addr", "add", "127.0.0.1/8", "dev", "lo")
		runCommandQuiet("ip", "-6", "addr", "add", "::1/128", "dev", "lo")
		return
	}
	if commandExists("ifconfig") {
		runCommandQuiet("ifconfig", "lo", "127.0.0.1", "up")
		runCommandQuiet("ifconfig", "lo", "inet6", "add", "::1/128")
	}
}

func setupLoopbackNative() bool {
	iface, err := net.InterfaceByName("lo")
	if err != nil {
		return false
	}
	if err := setInterfaceUp(iface.Name); err != nil {
		return false
	}
	if err := addInterfaceAddress(iface.Index, net.IPv4(127, 0, 0, 1).To4(), 8, unix.AF_INET, unix.RT_SCOPE_HOST, 0); err != nil {
		return false
	}
	if err := addInterfaceAddress(iface.Index, net.ParseIP("::1").To16(), 128, unix.AF_INET6, unix.RT_SCOPE_HOST, unix.IFA_F_NODAD); err != nil {
		return false
	}
	return true
}

func setInterfaceUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func addInterfaceAddress(index int, ip net.IP, prefixLen uint8, family uint8, scope uint8, flags uint8) error {
	conn, err := netlink.Dial(unix.NETLINK_ROUTE, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.Execute(netlink.Message{
		Header: netlink.Header{
			Type:  unix.RTM_NEWADDR,
			Flags: netlink.Request | netlink.Acknowledge | netlink.Create | netlink.Excl,
		},
		Data: interfaceAddressMessageData(index, ip, prefixLen, family, scope, flags),
	})
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	return err
}

func interfaceAddressMessageData(index int, ip net.IP, prefixLen uint8, family uint8, scope uint8, flags uint8) []byte {
	msg := marshalIfAddrmsg(unix.IfAddrmsg{
		Family:    family,
		Prefixlen: prefixLen,
		Flags:     flags,
		Scope:     scope,
		Index:     uint32(index),
	})
	attrs, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.IFA_LOCAL, Data: ip},
		{Type: unix.IFA_ADDRESS, Data: ip},
	})
	if err != nil {
		return msg
	}
	return append(msg, attrs...)
}

func marshalIfAddrmsg(msg unix.IfAddrmsg) []byte {
	b := make([]byte, unix.SizeofIfAddrmsg)
	b[0] = msg.Family
	b[1] = msg.Prefixlen
	b[2] = msg.Flags
	b[3] = msg.Scope
	nlenc.PutUint32(b[4:8], msg.Index)
	return b
}

func configureVMNetStaticNetwork(cmdline, iface string) bool {
	guestIPv4, okGuest := kernelCmdlineValue(cmdline, "cleanroom_vmnet_guest_ipv4")
	gatewayIPv4, okGateway := kernelCmdlineValue(cmdline, "cleanroom_vmnet_gateway_ipv4")
	prefixLen, okPrefix := kernelCmdlineValue(cmdline, "cleanroom_vmnet_prefix_len")
	if !okGuest || !okGateway || !okPrefix {
		return false
	}

	if configureVMNetStaticNetworkNative(iface, guestIPv4, gatewayIPv4, prefixLen) {
		return true
	}

	if commandExists("ip") {
		if !runCommandQuiet("ip", "link", "set", iface, "up") {
			return false
		}
		runCommandQuiet("ip", "addr", "flush", "dev", iface, "scope", "global")
		if !runCommandQuiet("ip", "addr", "add", guestIPv4+"/"+prefixLen, "dev", iface) {
			return false
		}
		if !runCommandQuiet("ip", "route", "replace", "default", "via", gatewayIPv4, "dev", iface) {
			return false
		}
		writeGuestResolver(gatewayIPv4)
		return true
	}

	if commandExists("ifconfig") {
		if !commandExists("route") {
			return false
		}
		prefix, err := strconv.Atoi(prefixLen)
		if err != nil {
			return false
		}
		netmask, ok := prefixToIPv4Mask(prefix)
		if !ok {
			return false
		}
		if !runCommandQuiet("ifconfig", iface, guestIPv4, "netmask", netmask, "up") {
			return false
		}
		runCommandQuiet("route", "del", "default")
		if !runCommandQuiet("route", "add", "default", "gw", gatewayIPv4) {
			return false
		}
		writeGuestResolver(gatewayIPv4)
		return true
	}

	return false
}

func configureVMNetStaticNetworkNative(iface, guestIPv4, gatewayIPv4, prefixLen string) bool {
	networkInterface, err := net.InterfaceByName(iface)
	if err != nil {
		return false
	}
	guestIP := net.ParseIP(guestIPv4).To4()
	if guestIP == nil {
		return false
	}
	gatewayIP := net.ParseIP(gatewayIPv4).To4()
	if gatewayIP == nil {
		return false
	}
	prefix, err := strconv.Atoi(prefixLen)
	if err != nil || prefix < 0 || prefix > 32 {
		return false
	}

	if err := setInterfaceUp(iface); err != nil {
		return false
	}
	if err := addInterfaceAddress(networkInterface.Index, guestIP, uint8(prefix), unix.AF_INET, unix.RT_SCOPE_UNIVERSE, 0); err != nil {
		return false
	}
	if err := replaceDefaultIPv4Route(networkInterface.Index, gatewayIP); err != nil {
		return false
	}
	writeGuestResolver(gatewayIPv4)
	return true
}

func replaceDefaultIPv4Route(index int, gateway net.IP) error {
	conn, err := netlink.Dial(unix.NETLINK_ROUTE, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.Execute(netlink.Message{
		Header: netlink.Header{
			Type:  unix.RTM_NEWROUTE,
			Flags: netlink.Request | netlink.Acknowledge | netlink.Create | netlink.Replace,
		},
		Data: defaultIPv4RouteMessageData(index, gateway),
	})
	return err
}

func defaultIPv4RouteMessageData(index int, gateway net.IP) []byte {
	msg := marshalRtMsg(unix.RtMsg{
		Family:   unix.AF_INET,
		Table:    unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_BOOT,
		Scope:    unix.RT_SCOPE_UNIVERSE,
		Type:     unix.RTN_UNICAST,
	})
	attrs, err := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.RTA_GATEWAY, Data: gateway.To4()},
		{Type: unix.RTA_OIF, Data: nlenc.Uint32Bytes(uint32(index))},
	})
	if err != nil {
		return msg
	}
	return append(msg, attrs...)
}

func marshalRtMsg(msg unix.RtMsg) []byte {
	b := make([]byte, unix.SizeofRtMsg)
	b[0] = msg.Family
	b[1] = msg.Dst_len
	b[2] = msg.Src_len
	b[3] = msg.Tos
	b[4] = msg.Table
	b[5] = msg.Protocol
	b[6] = msg.Scope
	b[7] = msg.Type
	nlenc.PutUint32(b[8:12], msg.Flags)
	return b
}

func writeGuestResolver(gatewayIPv4 string) {
	if err := os.MkdirAll("/etc", 0o755); err != nil {
		return
	}
	_ = os.WriteFile("/etc/resolv.conf", []byte("nameserver "+gatewayIPv4+"\n"), 0o644)
}

func listNetworkInterfaces() []string {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func startStdioGuestAgentIfPresent() {
	dev := firstCharacterDevice([]string{"/dev/hvc1", "/dev/vport1p0"}, os.Stat)
	if dev == "" {
		return
	}
	runSttyRawNoEcho(dev)
	go superviseStdioGuestAgent(dev)
}

func runSttyRawNoEcho(dev string) {
	if !commandExists("stty") {
		return
	}
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	cmd := exec.Command("stty", "raw", "-echo")
	cmd.Stdin = f
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}

func superviseStdioGuestAgent(dev string) {
	for {
		runStdioGuestAgent(dev)
		time.Sleep(time.Second)
	}
}

func runStdioGuestAgent(dev string) {
	self := guestAgentExecutablePath()
	device, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer device.Close()

	stderr := io.Writer(io.Discard)
	if console, err := os.OpenFile("/dev/hvc0", os.O_WRONLY|os.O_APPEND, 0); err == nil {
		defer console.Close()
		stderr = console
	}

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "CLEANROOM_GUEST_TRANSPORT=stdio")
	cmd.Stdin = device
	cmd.Stdout = device
	cmd.Stderr = stderr
	_ = cmd.Run()
}

func guestAgentExecutablePath() string {
	if self, err := os.Executable(); err == nil && strings.TrimSpace(self) != "" {
		return self
	}
	if len(os.Args) > 0 && strings.TrimSpace(os.Args[0]) != "" {
		return os.Args[0]
	}
	return guestAgentInstalledPath
}

func enableGuestInitBootTimings(cmdline string) {
	if value, ok := kernelCmdlineValue(cmdline, "cleanroom_guest_boot_timing"); !ok || value != "1" {
		return
	}
	initMS, err := readGuestBootUptimeMS()
	if err != nil {
		return
	}
	guestBootTimings = newGuestBootTimingStoreFromInitial(
		map[string]int64{vsockexec.GuestBootTimingInitVSOCKAgentExec: initMS},
		newGuestBootTimingClock(),
	)
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func runCommandQuiet(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

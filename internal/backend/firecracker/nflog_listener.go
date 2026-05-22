//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/netip"
	"strings"
	"time"

	charmlog "charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/dnsproxy"
	nflog "github.com/florianl/go-nflog/v2"
	"github.com/mdlayher/netlink"
)

// nflogGroupFromTapName derives an NFLOG group number from a tap device name
// using FNV32a, mapped to the range 100–65535.
func nflogGroupFromTapName(tapName string) uint16 {
	h := fnv.New32a()
	h.Write([]byte(tapName))
	return uint16(100 + h.Sum32()%(65535-100+1))
}

type nflogListenerConfig struct {
	group     uint16
	sandboxID string
	guestIP   netip.Addr
	runtime   *dnsproxy.Runtime
	logger    *charmlog.Logger
	onBlocked func(string)
}

type nflogListener struct {
	nf     *nflog.Nflog
	cancel context.CancelFunc
}

func newNFLogListener(cfg nflogListenerConfig) (*nflogListener, error) {
	config := nflog.Config{
		Group:    cfg.group,
		Copymode: nflog.CopyPacket,
		Bufsize:  128,
	}

	nf, err := nflog.Open(&config)
	if err != nil {
		return nil, fmt.Errorf("nflog open group %d: %w", cfg.group, err)
	}

	if err := nf.Con.SetReadBuffer(64 * 1024); err != nil {
		nf.Close()
		return nil, fmt.Errorf("nflog set read buffer: %w", err)
	}

	if err := nf.SetOption(netlink.NoENOBUFS, true); err != nil {
		nf.Close()
		return nil, fmt.Errorf("nflog set NoENOBUFS: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	hook := func(attrs nflog.Attribute) int {
		if attrs.Payload == nil {
			return 0
		}
		destIP, destPort, _, ok := parseIPv4PacketHeader(*attrs.Payload)
		if !ok {
			return 0
		}

		names := cfg.runtime.NamesForAddress(cfg.sandboxID, cfg.guestIP, destIP, time.Now())
		var dest string
		if len(names) > 0 {
			dest = strings.Join(names, ",")
		} else {
			dest = destIP.String()
		}

		msg := fmt.Sprintf("network connection blocked: %s:%d", dest, destPort)
		cfg.onBlocked(msg)
		return 0
	}

	errFunc := func(e error) int {
		if ctx.Err() != nil {
			return 1
		}
		baseFirecrackerLogger(cfg.logger).Warn("nflog listener error", "nflog_group", cfg.group, "error", e)
		return 0
	}

	if err := nf.RegisterWithErrorFunc(ctx, hook, errFunc); err != nil {
		cancel()
		nf.Close()
		return nil, fmt.Errorf("nflog register hook: %w", err)
	}

	return &nflogListener{
		nf:     nf,
		cancel: cancel,
	}, nil
}

func (l *nflogListener) Close() error {
	l.cancel()
	return l.nf.Close()
}

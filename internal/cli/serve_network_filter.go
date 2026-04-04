package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/buildkite/cleanroom/internal/networkfilterstate"
)

type ServeNetworkFilterCommand struct {
	Listen   string `help:"Listen address for the network-filter state daemon" hidden:"" env:"CLEANROOM_NETWORK_FILTER_DAEMON_LISTEN"`
	StateDir string `help:"State directory for persisted network-filter data" hidden:"" env:"CLEANROOM_NETWORK_FILTER_STATE_DIR"`
}

func (c *ServeNetworkFilterCommand) Run(_ *runtimeContext) error {
	stateDir := strings.TrimSpace(c.StateDir)
	if stateDir == "" {
		stateDir = networkfilterstate.DefaultStateDir
	}
	store, err := networkfilterstate.NewStore(stateDir)
	if err != nil {
		return err
	}

	listen := strings.TrimSpace(c.Listen)
	if listen == "" {
		listen = networkfilterstate.DefaultListenAddress
	}
	if shouldShowStartupHeader(os.Stderr) {
		if err := writeStartupHeader(os.Stderr, startupHeader{
			Title: "cleanroom serve-network-filter",
			Fields: []startupField{
				{Key: "listen", Value: listen},
				{Key: "state_dir", Value: stateDir},
			},
		}, shouldUseANSI(os.Stderr)); err != nil {
			return err
		}
	}

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := networkfilterstate.ListenAndServe(runCtx, listen, networkfilterstate.NewServer(store)); err != nil {
		return fmt.Errorf("serve network filter daemon: %w", err)
	}
	return nil
}

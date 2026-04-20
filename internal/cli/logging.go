package cli

import (
	"fmt"
	"os"

	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/charmbracelet/log"
)

func newLogger(rawLevel string, cfg runtimeconfig.ObservabilityConfig, component string) (*log.Logger, error) {
	levelName := effectiveLogLevel(rawLevel)
	if _, err := log.ParseLevel(levelName); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: %w", rawLevel, err)
	}
	logger, err := observability.NewLogger(os.Stderr, levelName, cfg, observability.LogFieldComponent, component)
	if err != nil {
		return nil, err
	}
	return logger, nil
}

func newClientLogger() *log.Logger {
	logger, _ := newLogger("warn", runtimeconfig.ObservabilityConfig{}, "client")
	return logger
}

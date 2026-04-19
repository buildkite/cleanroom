package cli

import (
	"fmt"
	"os"

	"github.com/charmbracelet/log"
)

func newLogger(rawLevel, component string) (*log.Logger, error) {
	levelName := effectiveLogLevel(rawLevel)
	level, err := log.ParseLevel(levelName)
	if err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: %w", rawLevel, err)
	}
	logger := log.NewWithOptions(os.Stderr, log.Options{
		Level:     level,
		Formatter: log.TextFormatter,
	})
	return logger.With("component", component), nil
}

func newClientLogger() *log.Logger {
	logger, _ := newLogger("warn", "client")
	return logger
}

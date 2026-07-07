package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	ccserver "github.com/buildkite/content-cache/server"
)

const defaultContentCacheListen = "127.0.0.1:8128"

var defaultContentCacheGitHosts = []string{"github.com"}
var defaultContentCacheFetchHosts = []string{"dl.google.com"}

type ContentCommand struct {
	Serve ContentCacheServeCommand `cmd:"" help:"Serve a persistent host content-cache daemon"`
}

type ContentCacheServeCommand struct {
	Listen            string        `help:"Loopback address to listen on" default:"127.0.0.1:8128"`
	Storage           string        `help:"Storage directory (default: user cache dir/cleanroom/content-cache)"`
	GitAllowedHosts   []string      `help:"Comma-separated Git upstream hosts to cache" sep:","`
	FetchAllowedHosts []string      `help:"Comma-separated /fetch upstream hosts to cache" sep:","`
	NoDefaultHosts    bool          `help:"Do not apply default Git/fetch host allowlists" hidden:""`
	CacheMaxSize      int64         `help:"Maximum cache size in bytes (0 disables size eviction)" default:"10737418240"`
	BlobRetention     time.Duration `help:"Minimum blob retention after last access" default:"24h"`
	LogLevel          string        `help:"Log level" enum:"debug,info,warn,error" default:"info"`
}

func (c *ContentCacheServeCommand) Run(ctx *runtimeContext) error {
	storage := c.Storage
	if storage == "" {
		var err error
		storage, err = defaultContentCacheStorage()
		if err != nil {
			return err
		}
	}
	logger := slog.New(slog.NewTextHandler(ctx.stderr(), &slog.HandlerOptions{Level: slogLevel(c.LogLevel)}))
	srv, err := ccserver.New(ccserver.Config{
		Address:               c.Listen,
		StoragePath:           storage,
		GitAllowedHosts:       contentCacheAllowedHosts(c.GitAllowedHosts, defaultContentCacheGitHosts, c.NoDefaultHosts),
		FetchAllowedHosts:     contentCacheAllowedHosts(c.FetchAllowedHosts, defaultContentCacheFetchHosts, c.NoDefaultHosts),
		GitMaxRequestBodySize: 100 << 20,
		GoProxyMetadataTTL:    24 * time.Hour,
		NPMMetadataTTL:        24 * time.Hour,
		OCIPrefix:             "docker-hub",
		FetchMetadataTTL:      24 * time.Hour,
		HTTPCacheTTL:          24 * time.Hour,
		BlobRetention:         c.BlobRetention,
		CacheMaxSize:          c.CacheMaxSize,
		GCInterval:            time.Hour,
		GCStartupDelay:        5 * time.Minute,
		Logger:                logger,
	})
	if err != nil {
		return fmt.Errorf("create content-cache server: %w", err)
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	if _, err := fmt.Fprintf(ctx.Stdout, "cleanroom content-cache: serving on http://%s with storage %s\n", c.Listen, storage); err != nil {
		return err
	}
	select {
	case err := <-errCh:
		return err
	case <-signalCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func defaultContentCacheStorage() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache dir: %w", err)
		}
	}
	return filepath.Join(base, "cleanroom", "content-cache"), nil
}

func slogLevel(value string) slog.Level {
	switch strings.TrimSpace(value) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func defaultStrings(values, defaults []string) []string {
	if len(values) == 0 {
		return append([]string(nil), defaults...)
	}
	return values
}

func contentCacheAllowedHosts(values, defaults []string, noDefaults bool) []string {
	if noDefaults {
		return append([]string(nil), values...)
	}
	return defaultStrings(values, defaults)
}

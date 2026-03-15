package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
)

const defaultRequestTimeout = 45 * time.Second

func main() {
	var (
		host      string
		tlsCA     string
		sandboxID string
		path      string
		maxBytes  int64
		timeout   time.Duration
	)

	flag.StringVar(&host, "host", "", "Control-plane endpoint")
	flag.StringVar(&tlsCA, "tls-ca", "", "Path to CA certificate for HTTPS hosts")
	flag.StringVar(&sandboxID, "sandbox-id", "", "Sandbox ID")
	flag.StringVar(&path, "path", "", "Absolute guest path to download")
	flag.Int64Var(&maxBytes, "max-bytes", 10*1024*1024, "Maximum bytes to download")
	flag.DurationVar(&timeout, "timeout", defaultRequestTimeout, "Request timeout (0 disables timeout)")
	flag.Parse()

	if strings.TrimSpace(host) == "" {
		fail("missing --host")
	}
	if strings.TrimSpace(sandboxID) == "" {
		fail("missing --sandbox-id")
	}
	if strings.TrimSpace(path) == "" {
		fail("missing --path")
	}
	if timeout < 0 {
		fail("invalid --timeout: must be >= 0")
	}

	ep, err := endpoint.Resolve(host)
	if err != nil {
		fail("resolve host: %v", err)
	}
	client, err := controlclient.New(ep, controlclient.WithTLS(tlsconfig.Options{
		CAPath: tlsCA,
	}))
	if err != nil {
		fail("create control client: %v", err)
	}

	ctx, cancel := requestContext(timeout)
	defer cancel()

	resp, err := client.DownloadSandboxFile(ctx, &cleanroomv1.DownloadSandboxFileRequest{
		SandboxId: sandboxID,
		Path:      path,
		MaxBytes:  maxBytes,
	})
	if err != nil {
		if timeout > 0 && errors.Is(err, context.DeadlineExceeded) {
			fail("download sandbox file timed out after %s: %v", timeout, err)
		}
		fail("download sandbox file: %v", err)
	}
	if _, err := os.Stdout.Write(resp.GetData()); err != nil {
		fail("write download payload: %v", err)
	}
}

func requestContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), timeout)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

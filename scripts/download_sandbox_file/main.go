package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var (
		host      string
		tlsCA     string
		sandboxID string
		path      string
		maxBytes  int64
		timeout   time.Duration
	)

	flags := flag.NewFlagSet("download-sandbox-file", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&host, "host", "", "Control-plane endpoint")
	flags.StringVar(&tlsCA, "tls-ca", "", "Path to CA certificate for HTTPS hosts")
	flags.StringVar(&sandboxID, "sandbox-id", "", "Sandbox ID")
	flags.StringVar(&path, "path", "", "Absolute guest path to download")
	flags.Int64Var(&maxBytes, "max-bytes", 10*1024*1024, "Maximum bytes to download")
	flags.DurationVar(&timeout, "timeout", defaultRequestTimeout, "Request timeout (0 disables timeout)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if strings.TrimSpace(host) == "" {
		return failf(stderr, "missing --host")
	}
	if strings.TrimSpace(sandboxID) == "" {
		return failf(stderr, "missing --sandbox-id")
	}
	if strings.TrimSpace(path) == "" {
		return failf(stderr, "missing --path")
	}
	if timeout < 0 {
		return failf(stderr, "invalid --timeout: must be >= 0")
	}

	ep, err := endpoint.Resolve(host)
	if err != nil {
		return failf(stderr, "resolve host: %v", err)
	}
	client, err := controlclient.New(ep, controlclient.WithTLS(tlsconfig.Options{
		CAPath: tlsCA,
	}))
	if err != nil {
		return failf(stderr, "create control client: %v", err)
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
			return failf(stderr, "download sandbox file timed out after %s: %v", timeout, err)
		}
		return failf(stderr, "download sandbox file: %v", err)
	}
	if _, err := stdout.Write(resp.GetData()); err != nil {
		return failf(stderr, "write download payload: %v", err)
	}
	return 0
}

func requestContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), timeout)
}

func failf(w io.Writer, format string, args ...any) int {
	fmt.Fprintf(w, format+"\n", args...)
	return 1
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
)

func main() {
	var (
		host      string
		tlsCA     string
		sandboxID string
		path      string
		maxBytes  int64
	)

	flag.StringVar(&host, "host", "", "Control-plane endpoint")
	flag.StringVar(&tlsCA, "tls-ca", "", "Path to CA certificate for HTTPS hosts")
	flag.StringVar(&sandboxID, "sandbox-id", "", "Sandbox ID")
	flag.StringVar(&path, "path", "", "Absolute guest path to download")
	flag.Int64Var(&maxBytes, "max-bytes", 10*1024*1024, "Maximum bytes to download")
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

	resp, err := client.DownloadSandboxFile(context.Background(), &cleanroomv1.DownloadSandboxFileRequest{
		SandboxId: sandboxID,
		Path:      path,
		MaxBytes:  maxBytes,
	})
	if err != nil {
		fail("download sandbox file: %v", err)
	}
	if _, err := os.Stdout.Write(resp.GetData()); err != nil {
		fail("write download payload: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

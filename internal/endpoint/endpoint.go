package endpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Endpoint struct {
	Scheme  string
	Address string
	BaseURL string
}

func Default() Endpoint {
	runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if runtimeDir == "" {
		runtimeDir = filepath.Join(os.TempDir(), "cleanroom")
	}
	sock := filepath.Join(runtimeDir, "cleanroom", "cleanroom.sock")
	return Endpoint{
		Scheme:  "unix",
		Address: sock,
		BaseURL: "http://unix",
	}
}

func Resolve(raw string) (Endpoint, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = strings.TrimSpace(os.Getenv("CLEANROOM_HOST"))
	}
	if value == "" {
		return Default(), nil
	}

	switch {
	case strings.HasPrefix(value, "unix://"):
		path := strings.TrimPrefix(value, "unix://")
		if path == "" {
			return Endpoint{}, fmt.Errorf("invalid unix endpoint %q", value)
		}
		return Endpoint{Scheme: "unix", Address: path, BaseURL: "http://unix"}, nil
	case strings.HasPrefix(value, "http://"), strings.HasPrefix(value, "https://"):
		scheme := "http"
		if strings.HasPrefix(value, "https://") {
			scheme = "https"
		}
		return Endpoint{Scheme: scheme, Address: value, BaseURL: value}, nil
	case strings.HasPrefix(value, "/"):
		return Endpoint{Scheme: "unix", Address: value, BaseURL: "http://unix"}, nil
	default:
		return Endpoint{}, fmt.Errorf("unsupported endpoint %q (expected unix://, http://, https://, or absolute unix socket path)", value)
	}
}

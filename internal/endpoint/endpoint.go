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

const DefaultSystemSocketPath = "/var/run/cleanroom/cleanroom.sock"

const defaultSystemSocketPath = DefaultSystemSocketPath

var endpointStat = os.Stat
var endpointGeteuid = os.Geteuid

func defaultListenEndpoint() Endpoint {
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

func defaultClientEndpoint() Endpoint {
	if endpointGeteuid() == 0 {
		if st, err := endpointStat(defaultSystemSocketPath); err == nil && !st.IsDir() && st.Mode()&os.ModeSocket != 0 {
			return Endpoint{
				Scheme:  "unix",
				Address: defaultSystemSocketPath,
				BaseURL: "http://unix",
			}
		}
	}
	return defaultListenEndpoint()
}

func Default() Endpoint {
	return defaultListenEndpoint()
}

// ResolveListen resolves an endpoint for server-side listening.
func ResolveListen(raw string) (Endpoint, error) {
	return resolve(raw, true)
}

func Resolve(raw string) (Endpoint, error) {
	return resolve(raw, false)
}

func resolve(raw string, listenDefault bool) (Endpoint, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = strings.TrimSpace(os.Getenv("CLEANROOM_HOST"))
	}
	if value == "" {
		if listenDefault {
			return defaultListenEndpoint(), nil
		}
		return defaultClientEndpoint(), nil
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
		expected := "unix://, http://, https://, or absolute unix socket path"
		return Endpoint{}, fmt.Errorf("unsupported endpoint %q (expected %s)", value, expected)
	}
}

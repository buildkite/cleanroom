package endpoint

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Endpoint struct {
	Scheme        string
	Address       string
	BaseURL       string
	TSNetHostname string
	TSNetPort     int
	TSServiceName string
	TSServicePort int
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

// ResolveListen resolves an endpoint for server-side listening, which supports
// tsnet:// in addition to all client-side schemes.
func ResolveListen(raw string) (Endpoint, error) {
	return resolve(raw, true)
}

func Resolve(raw string) (Endpoint, error) {
	return resolve(raw, false)
}

func resolve(raw string, allowTSNet bool) (Endpoint, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		if allowTSNet {
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
	case strings.HasPrefix(value, "tsnet://"):
		if !allowTSNet {
			return Endpoint{}, fmt.Errorf("tsnet:// endpoints are only valid for server --listen; use http://HOSTNAME.tailnet.ts.net:PORT for client connections")
		}
		return resolveTSNet(value)
	case strings.HasPrefix(value, "tssvc://"):
		return resolveTailscaleService(value)
	case strings.HasPrefix(value, "http://"), strings.HasPrefix(value, "https://"):
		scheme := "http"
		if strings.HasPrefix(value, "https://") {
			scheme = "https"
		}
		return Endpoint{Scheme: scheme, Address: value, BaseURL: value}, nil
	case strings.HasPrefix(value, "/"):
		return Endpoint{Scheme: "unix", Address: value, BaseURL: "http://unix"}, nil
	default:
		expected := "unix://, tssvc://, http://, https://, or absolute unix socket path"
		if allowTSNet {
			expected = "unix://, tsnet://, tssvc://, http://, https://, or absolute unix socket path"
		}
		return Endpoint{}, fmt.Errorf("unsupported endpoint %q (expected %s)", value, expected)
	}
}

func resolveTSNet(value string) (Endpoint, error) {
	const defaultHostname = "cleanroom"
	const defaultPort = 7777

	u, err := url.Parse(value)
	if err != nil {
		return Endpoint{}, fmt.Errorf("invalid tsnet endpoint %q: %w", value, err)
	}
	if u.Path != "" && u.Path != "/" {
		return Endpoint{}, fmt.Errorf("invalid tsnet endpoint %q: path is not supported", value)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Endpoint{}, fmt.Errorf("invalid tsnet endpoint %q: query and fragment are not supported", value)
	}

	hostname := strings.TrimSpace(u.Hostname())
	if hostname == "" {
		hostname = defaultHostname
	}

	port := u.Port()
	if port == "" {
		port = strconv.Itoa(defaultPort)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return Endpoint{}, fmt.Errorf("invalid tsnet endpoint %q: port must be in range 1-65535", value)
	}

	return Endpoint{
		Scheme:        "tsnet",
		Address:       fmt.Sprintf(":%d", portNum),
		BaseURL:       fmt.Sprintf("http://%s:%d", hostname, portNum),
		TSNetHostname: hostname,
		TSNetPort:     portNum,
	}, nil
}

func resolveTailscaleService(value string) (Endpoint, error) {
	const defaultServiceLabel = "cleanroom"
	const defaultLocalPort = 7777

	u, err := url.Parse(value)
	if err != nil {
		return Endpoint{}, fmt.Errorf("invalid tssvc endpoint %q: %w", value, err)
	}
	if u.Path != "" && u.Path != "/" {
		return Endpoint{}, fmt.Errorf("invalid tssvc endpoint %q: path is not supported", value)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Endpoint{}, fmt.Errorf("invalid tssvc endpoint %q: query and fragment are not supported", value)
	}

	label := strings.TrimSpace(strings.ToLower(u.Hostname()))
	if label == "" {
		label = defaultServiceLabel
	}
	if !isDNSLabel(label) {
		return Endpoint{}, fmt.Errorf("invalid tssvc endpoint %q: service label must be a valid DNS label", value)
	}

	port := u.Port()
	if port == "" {
		port = strconv.Itoa(defaultLocalPort)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return Endpoint{}, fmt.Errorf("invalid tssvc endpoint %q: port must be in range 1-65535", value)
	}

	return Endpoint{
		Scheme:        "tssvc",
		Address:       fmt.Sprintf("127.0.0.1:%d", portNum),
		BaseURL:       fmt.Sprintf("https://%s.<tailnet>.ts.net", label),
		TSServiceName: fmt.Sprintf("svc:%s", label),
		TSServicePort: portNum,
	}, nil
}

func isDNSLabel(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
		if isAlphaNum {
			continue
		}
		if ch != '-' {
			return false
		}
		if i == 0 || i == len(value)-1 {
			return false
		}
	}
	return true
}

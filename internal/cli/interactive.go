package cli

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/endpoint"
	"golang.org/x/term"
)

func normalizeLineEndingsForRawTTY(chunk []byte, prevEndedCR bool) ([]byte, bool) {
	if len(chunk) == 0 {
		return chunk, prevEndedCR
	}

	if bytes.IndexByte(chunk, '\n') < 0 {
		return chunk, chunk[len(chunk)-1] == '\r'
	}

	out := make([]byte, 0, len(chunk)+4)
	endedCR := prevEndedCR
	for _, b := range chunk {
		if b == '\n' && !endedCR {
			out = append(out, '\r')
		}
		out = append(out, b)
		endedCR = b == '\r'
	}
	return out, endedCR
}

func attachTTYSize(fd int) (uint32, uint32) {
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return 80, 24
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return uint32(cols), uint32(rows)
}

func trimPassthroughSeparator(args []string) []string {
	if len(args) == 0 {
		return args
	}
	if strings.TrimSpace(args[0]) != "--" {
		return args
	}
	out := make([]string, 0, len(args)-1)
	out = append(out, args[1:]...)
	return out
}

func resolveInteractiveQUICEndpoint(ep endpoint.Endpoint) (listenAddr, advertiseHost string) {
	if ep.Scheme == "unix" {
		return "127.0.0.1:0", "127.0.0.1"
	}

	address := strings.TrimSpace(ep.Address)
	address = strings.TrimPrefix(address, "http://")
	address = strings.TrimPrefix(address, "https://")
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.TrimSpace(port) == "" {
		return "127.0.0.1:0", "127.0.0.1"
	}

	normalizedHost := host
	if normalizedHost == "" {
		normalizedHost = "0.0.0.0"
	}

	advertiseHost = strings.TrimSpace(host)
	return net.JoinHostPort(normalizedHost, port), advertiseHost
}

func interactiveAdvertiseEndpoint(listenerAddr net.Addr, advertiseHost string) string {
	if listenerAddr == nil {
		return ""
	}
	host, port, err := net.SplitHostPort(listenerAddr.String())
	if err != nil || strings.TrimSpace(port) == "" {
		return listenerAddr.String()
	}
	targetHost := strings.TrimSpace(advertiseHost)
	if targetHost == "" {
		targetHost = host
	}
	return net.JoinHostPort(targetHost, port)
}

func resolveInteractiveDialEndpoint(controlEP endpoint.Endpoint, quicEndpoint string) string {
	quicEndpoint = strings.TrimSpace(quicEndpoint)
	if quicEndpoint == "" {
		return quicEndpoint
	}
	quicHost, quicPort, err := net.SplitHostPort(quicEndpoint)
	if err != nil || strings.TrimSpace(quicPort) == "" {
		return quicEndpoint
	}
	if !isWildcardOrLoopbackHost(quicHost) {
		return quicEndpoint
	}

	controlHost := resolveEndpointDialHost(controlEP)
	if controlHost == "" || isWildcardHost(controlHost) {
		return quicEndpoint
	}
	return net.JoinHostPort(controlHost, quicPort)
}

func resolveEndpointDialHost(ep endpoint.Endpoint) string {
	if ep.Scheme == "unix" {
		return ""
	}
	baseURL := strings.TrimSpace(ep.BaseURL)
	if baseURL == "" {
		return ""
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Hostname())
}

func isWildcardOrLoopbackHost(host string) bool {
	return isWildcardHost(host) || isLoopbackHost(host)
}

func isWildcardHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	switch host {
	case "", "0.0.0.0", "::":
		return true
	default:
		return false
	}
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func pollInteractiveExitOrControlErr(exitCodeCh <-chan int, controlErrCh *chan error) (int, bool, error) {
	if controlErrCh == nil || *controlErrCh == nil {
		if exitCodeCh == nil {
			return 0, false, nil
		}
		select {
		case code := <-exitCodeCh:
			return code, true, nil
		default:
			return 0, false, nil
		}
	}

	select {
	case controlErr, ok := <-*controlErrCh:
		if !ok {
			*controlErrCh = nil
			if exitCodeCh == nil {
				return 0, false, nil
			}
			select {
			case code := <-exitCodeCh:
				return code, true, nil
			default:
				return 0, false, nil
			}
		}
		if controlErr != nil && !isInteractiveStreamClosedErr(controlErr) {
			return 0, false, fmt.Errorf("interactive control stream: %w", controlErr)
		}
	default:
	}
	if exitCodeCh == nil {
		return 0, false, nil
	}
	select {
	case code := <-exitCodeCh:
		return code, true, nil
	default:
	}
	return 0, false, nil
}

func waitForInteractiveExitOrControlErr(exitCodeCh <-chan int, controlErrCh *chan error, timeout time.Duration) (int, bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		if code, haveExitCode, pollErr := pollInteractiveExitOrControlErr(exitCodeCh, controlErrCh); pollErr != nil || haveExitCode {
			return code, haveExitCode, pollErr
		}

		if controlErrCh == nil || *controlErrCh == nil {
			select {
			case code := <-exitCodeCh:
				return code, true, nil
			case <-deadline.C:
				return 0, false, nil
			}
		}

		select {
		case code := <-exitCodeCh:
			return code, true, nil
		case controlErr, ok := <-*controlErrCh:
			if !ok {
				*controlErrCh = nil
				continue
			}
			if controlErr != nil && !isInteractiveStreamClosedErr(controlErr) {
				return 0, false, fmt.Errorf("interactive control stream: %w", controlErr)
			}
		case <-deadline.C:
			return 0, false, nil
		}
	}
}

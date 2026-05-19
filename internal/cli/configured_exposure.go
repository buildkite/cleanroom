package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/exposure"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

func normalizeBareExposeHTTPSArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		arg := out[i]
		if arg == "--" {
			break
		}
		if isPartialPassthroughCommand(out) && i > 0 && !strings.HasPrefix(arg, "-") {
			break
		}
		if arg != "--expose-https" {
			if isPartialPassthroughCommand(out) && cliFlagConsumesNextArg(arg) && !strings.Contains(arg, "=") && i+1 < len(out) {
				i++
			}
			continue
		}
		if i+1 >= len(out) || out[i+1] == "--" || strings.HasPrefix(out[i+1], "-") {
			out[i] = "--expose-https=" + configuredHTTPSExposureSpec
			continue
		}
		if isPartialPassthroughCommand(out) {
			if !looksLikeHTTPSExposureSpecValue(out[i+1]) {
				out[i] = "--expose-https=" + configuredHTTPSExposureSpec
				continue
			}
			i++
		}
	}
	return out
}

func isPartialPassthroughCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "exec", "console":
		return true
	default:
		return false
	}
}

func cliFlagConsumesNextArg(arg string) bool {
	switch arg {
	case "--backend",
		"--chdir",
		"--env",
		"--expose",
		"--expose-https",
		"--from",
		"--host",
		"--image",
		"--in",
		"--launch-seconds",
		"--repo-commit",
		"--repo-url",
		"--sandbox-id",
		"--tls-ca",
		"--tlsca",
		"-c",
		"-e":
		return true
	default:
		return false
	}
}

func looksLikeHTTPSExposureSpecValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.Contains(value, ":") {
		return true
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func resolveRequestedExposures(ctx *runtimeContext, cwd, sandboxID string, requested []*cleanroomv1.PortExposure) ([]*cleanroomv1.PortExposure, error) {
	if !hasConfiguredHTTPSExposure(requested) {
		return requested, nil
	}
	cfg, err := loadConfiguredHTTPSExposure(ctx, cwd)
	if err != nil {
		return nil, err
	}
	configured, err := expandConfiguredHTTPSExposures(cfg, sandboxID)
	if err != nil {
		return nil, err
	}

	resolved := make([]*cleanroomv1.PortExposure, 0, len(requested)+len(configured))
	for _, req := range requested {
		if isConfiguredHTTPSExposure(req) {
			resolved = append(resolved, configured...)
			continue
		}
		resolved = append(resolved, req)
	}
	return resolved, nil
}

func prevalidateConfiguredExposures(ctx *runtimeContext, cwd string, requested []*cleanroomv1.PortExposure) error {
	if !hasConfiguredHTTPSExposure(requested) {
		return nil
	}
	cfg, err := loadConfiguredHTTPSExposure(ctx, cwd)
	if err != nil {
		return err
	}
	_, err = expandConfiguredHTTPSExposures(cfg, policy.ExposeHTTPSPreflightSandboxID)
	return err
}

func loadConfiguredHTTPSExposure(ctx *runtimeContext, cwd string) (policy.ExposeHTTPSConfig, error) {
	if ctx == nil || ctx.Loader == nil {
		return policy.ExposeHTTPSConfig{}, errors.New("configured --expose-https requires a policy loader")
	}
	cfg, _, err := ctx.Loader.LoadExpose(cwd)
	if err != nil {
		if errors.Is(err, policy.ErrPolicyNotFound) {
			return policy.ExposeHTTPSConfig{}, errors.New("configured --expose-https requires expose.https in cleanroom.yaml")
		}
		return policy.ExposeHTTPSConfig{}, err
	}
	return cfg.HTTPS, nil
}

func hasConfiguredHTTPSExposure(requested []*cleanroomv1.PortExposure) bool {
	for _, req := range requested {
		if isConfiguredHTTPSExposure(req) {
			return true
		}
	}
	return false
}

func isConfiguredHTTPSExposure(req *cleanroomv1.PortExposure) bool {
	return req != nil &&
		strings.TrimSpace(req.GetProtocol()) == exposureProtocolHTTPS &&
		strings.TrimSpace(req.GetName()) == configuredHTTPSExposureSpec &&
		req.GetGuestPort() == 0
}

func expandConfiguredHTTPSExposures(cfg policy.ExposeHTTPSConfig, sandboxID string) ([]*cleanroomv1.PortExposure, error) {
	sandboxID = strings.TrimSpace(strings.ToLower(sandboxID))
	if sandboxID == "" {
		return nil, errors.New("configured --expose-https requires a sandbox id")
	}
	if cfg.IsZero() {
		return nil, errors.New("configured --expose-https requires expose.https in cleanroom.yaml")
	}
	base := strings.TrimSpace(strings.ToLower(cfg.Base))
	if base != "" {
		base = expandExposeTemplate(base, sandboxID, "")
		base = strings.TrimSpace(strings.ToLower(base))
	}
	if strings.TrimSpace(cfg.Base) != "" && base == "" {
		return nil, errors.New("expose.https.base expanded to an empty host")
	}

	exposures := make([]*cleanroomv1.PortExposure, 0)
	seenHosts := map[string]struct{}{}
	for i, route := range cfg.Routes {
		if route.Port < 1 || route.Port > 65535 {
			return nil, fmt.Errorf("expose.https.routes[%d].port must be in range 1-65535", i)
		}
		if len(route.Hosts) == 0 {
			return nil, fmt.Errorf("expose.https.routes[%d].hosts must include at least one host", i)
		}
		for j, host := range route.Hosts {
			if strings.Contains(host, "{base}") && base == "" {
				return nil, fmt.Errorf("expose.https.routes[%d].hosts[%d] uses {base} but expose.https.base is empty", i, j)
			}
			host = expandExposeTemplate(host, sandboxID, base)
			host = strings.TrimSpace(strings.ToLower(host))
			if host == "" {
				return nil, fmt.Errorf("expose.https.routes[%d].hosts[%d] expanded to an empty host", i, j)
			}
			if err := exposure.ValidateHTTPSRouteName(host); err != nil {
				return nil, fmt.Errorf("expose.https.routes[%d].hosts[%d] is invalid: %w", i, j, err)
			}
			if _, ok := seenHosts[host]; ok {
				return nil, fmt.Errorf("expose.https.routes[%d].hosts[%d] duplicates configured host %q", i, j, host)
			}
			seenHosts[host] = struct{}{}
			exposures = append(exposures, &cleanroomv1.PortExposure{
				Protocol:  exposureProtocolHTTPS,
				GuestPort: int32(route.Port),
				Name:      host,
			})
		}
	}
	return exposures, nil
}

func expandExposeTemplate(value, sandboxID, base string) string {
	value = strings.ReplaceAll(value, "{sandbox_id}", sandboxID)
	value = strings.ReplaceAll(value, "{container_id}", sandboxID)
	value = strings.ReplaceAll(value, "{base}", base)
	return value
}

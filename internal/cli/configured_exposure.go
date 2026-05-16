package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/exposure"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

const configuredHTTPSPreflightSandboxID = "cr-preflight"

func normalizeBareExposeHTTPSArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := append([]string(nil), args...)
	for i, arg := range out {
		if arg == "--" {
			break
		}
		if arg != "--expose-https" {
			continue
		}
		if i+1 >= len(out) || out[i+1] == "--" || strings.HasPrefix(out[i+1], "-") {
			out[i] = "--expose-https=" + configuredHTTPSExposureSpec
		}
	}
	return out
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
	_, err = expandConfiguredHTTPSExposures(cfg, configuredHTTPSPreflightSandboxID)
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

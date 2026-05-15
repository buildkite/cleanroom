package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

const (
	exposureProtocolTCP   = "tcp"
	exposureProtocolHTTPS = "https"

	configuredHTTPSExposureSpec = "__cleanroom_configured_https__"
)

func parseExposureFlags(tcpSpecs, httpsSpecs []string) ([]*cleanroomv1.PortExposure, error) {
	exposures := make([]*cleanroomv1.PortExposure, 0, len(tcpSpecs)+len(httpsSpecs))
	for _, spec := range tcpSpecs {
		exposure, err := parseTCPExposureSpec(spec)
		if err != nil {
			return nil, err
		}
		exposures = append(exposures, exposure)
	}
	for _, spec := range httpsSpecs {
		exposure, err := parseHTTPSExposureSpec(spec)
		if err != nil {
			return nil, err
		}
		exposures = append(exposures, exposure)
	}
	return exposures, nil
}

func writeExposureLines(w io.Writer, exposures []*cleanroomv1.PortExposure) error {
	if w == nil {
		return nil
	}
	for _, exposure := range exposures {
		if exposure == nil {
			continue
		}
		url := strings.TrimSpace(exposure.GetUrl())
		if url == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "exposed: %s\n", url); err != nil {
			return err
		}
	}
	return nil
}

func parseTCPExposureSpec(spec string) (*cleanroomv1.PortExposure, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("invalid --expose value: empty exposure")
	}

	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		guestPort, err := parseExposurePort(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid --expose value %q: %w", spec, err)
		}
		return &cleanroomv1.PortExposure{
			Protocol:  exposureProtocolTCP,
			HostPort:  int32(guestPort),
			GuestPort: int32(guestPort),
		}, nil
	case 2:
		hostPort, err := parseExposurePort(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid --expose value %q: host port: %w", spec, err)
		}
		guestPort, err := parseExposurePort(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid --expose value %q: guest port: %w", spec, err)
		}
		return &cleanroomv1.PortExposure{
			Protocol:  exposureProtocolTCP,
			HostPort:  int32(hostPort),
			GuestPort: int32(guestPort),
		}, nil
	default:
		return nil, fmt.Errorf("invalid --expose value %q: expected <guest-port> or <host-port>:<guest-port>", spec)
	}
}

func parseHTTPSExposureSpec(spec string) (*cleanroomv1.PortExposure, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = configuredHTTPSExposureSpec
	}
	if spec == configuredHTTPSExposureSpec {
		return &cleanroomv1.PortExposure{
			Protocol: exposureProtocolHTTPS,
			Name:     configuredHTTPSExposureSpec,
		}, nil
	}

	name := ""
	portSpec := spec
	if before, after, ok := strings.Cut(spec, ":"); ok {
		name = strings.TrimSpace(before)
		portSpec = strings.TrimSpace(after)
		if err := validateExposureName(name); err != nil {
			return nil, fmt.Errorf("invalid --expose-https value %q: %w", spec, err)
		}
		if strings.Contains(portSpec, ":") {
			return nil, fmt.Errorf("invalid --expose-https value %q: expected [name:]<guest-port>", spec)
		}
	}

	guestPort, err := parseExposurePort(portSpec)
	if err != nil {
		return nil, fmt.Errorf("invalid --expose-https value %q: %w", spec, err)
	}
	return &cleanroomv1.PortExposure{
		Protocol:  exposureProtocolHTTPS,
		GuestPort: int32(guestPort),
		Name:      name,
	}, nil
}

func parseExposurePort(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("missing port")
	}
	port, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("port must be numeric: %w", err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %d out of range 1-65535", port)
	}
	return port, nil
}

func validateExposureName(name string) error {
	if name == "" {
		return errors.New("missing route name")
	}
	if len(name) > 63 {
		return fmt.Errorf("route name %q is longer than 63 characters", name)
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("route name %q cannot start or end with '-'", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("route name %q must contain only lowercase letters, digits, and '-'", name)
		}
	}
	return nil
}

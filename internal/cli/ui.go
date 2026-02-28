package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"golang.org/x/term"
)

type startupHeader struct {
	Title  string
	Fields []startupField
}

type startupField struct {
	Key   string
	Value string
}

func renderStartupHeader(h startupHeader, color bool) string {
	title := strings.TrimSpace(h.Title)
	if title == "" {
		title = "cleanroom"
	}

	var out strings.Builder
	icon := "🧑‍🔬"
	if color {
		icon = ansiWrap("1;33", icon)
		title = ansiWrap("1;36", title)
	}

	out.WriteByte('\n')
	out.WriteString(icon)
	out.WriteString(" ")
	out.WriteString(title)
	out.WriteByte('\n')

	for _, field := range h.Fields {
		key := strings.TrimSpace(field.Key)
		value := strings.TrimSpace(field.Value)
		if key == "" || value == "" {
			continue
		}

		line := fmt.Sprintf("%s: %s", key, value)
		if color {
			line = ansiWrap("38;5;252", line)
		}
		out.WriteString("   ")
		out.WriteString(line)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')

	return out.String()
}

func renderDoctorReport(backendName string, checks []backend.DoctorCheck, color bool) string {
	name := strings.TrimSpace(backendName)
	if name == "" {
		name = "unknown"
	}

	var out strings.Builder
	title := fmt.Sprintf("doctor report (%s)", name)
	if color {
		title = ansiWrap("1;36", title)
	}
	out.WriteString(title)
	out.WriteByte('\n')

	passCount := 0
	warnCount := 0
	failCount := 0

	for _, check := range checks {
		status := normalizeDoctorStatus(check.Status)
		switch status {
		case "pass":
			passCount++
		case "warn":
			warnCount++
		case "fail":
			failCount++
		}

		icon := "?"
		switch status {
		case "pass":
			icon = "✓"
		case "warn":
			icon = "!"
		case "fail":
			icon = "✗"
		}

		statusBlock := fmt.Sprintf("%s [%s]", icon, status)
		if color {
			code := "1;37"
			switch status {
			case "pass":
				code = "1;32"
			case "warn":
				code = "1;33"
			case "fail":
				code = "1;31"
			}
			statusBlock = ansiWrap(code, statusBlock)
		}

		checkName := strings.TrimSpace(check.Name)
		if checkName == "" {
			checkName = "unnamed_check"
		}
		message := strings.TrimSpace(check.Message)
		if message == "" {
			message = "(no message)"
		}

		out.WriteString(statusBlock)
		out.WriteString(" ")
		out.WriteString(checkName)
		out.WriteString(": ")
		out.WriteString(message)
		out.WriteByte('\n')
	}

	summary := fmt.Sprintf("summary: %d pass, %d warn, %d fail", passCount, warnCount, failCount)
	if color {
		summary = ansiWrap("38;5;246", summary)
	}
	out.WriteString(summary)
	out.WriteByte('\n')

	return out.String()
}

func writeStartupHeader(w io.Writer, h startupHeader, color bool) error {
	if w == nil {
		return nil
	}
	_, err := io.WriteString(w, renderStartupHeader(h, color))
	return err
}

func shouldShowStartupHeader(stderr *os.File) bool {
	if stderr == nil {
		return false
	}
	return term.IsTerminal(int(stderr.Fd()))
}

func shouldUseANSI(stderr *os.File) bool {
	if noColorRequested() {
		return false
	}
	if forceColorRequested() {
		return true
	}
	if stderr == nil {
		return false
	}
	return term.IsTerminal(int(stderr.Fd()))
}

func applyPolishedLoggerStyles(logger *log.Logger, color bool) {
	if logger == nil || !color {
		return
	}

	styles := log.DefaultStyles()
	styles.Message = styles.Message.Foreground(lipgloss.Color("252"))
	styles.Key = styles.Key.Bold(true).Foreground(lipgloss.Color("75"))
	styles.Value = styles.Value.Foreground(lipgloss.Color("255"))
	styles.Separator = styles.Separator.Foreground(lipgloss.Color("240"))
	styles.Levels[log.DebugLevel] = styles.Levels[log.DebugLevel].Bold(true).Foreground(lipgloss.Color("45"))
	styles.Levels[log.InfoLevel] = styles.Levels[log.InfoLevel].Bold(true).Foreground(lipgloss.Color("48"))
	styles.Levels[log.WarnLevel] = styles.Levels[log.WarnLevel].Bold(true).Foreground(lipgloss.Color("214"))
	styles.Levels[log.ErrorLevel] = styles.Levels[log.ErrorLevel].Bold(true).Foreground(lipgloss.Color("203"))
	logger.SetStyles(styles)
}

func endpointDisplay(ep endpoint.Endpoint) string {
	switch ep.Scheme {
	case "unix":
		return "unix://" + ep.Address
	case "http", "https":
		if ep.Address != "" {
			return ep.Address
		}
		return ep.BaseURL
	case "tsnet":
		host := strings.TrimSpace(ep.TSNetHostname)
		if host == "" {
			host = "cleanroom"
		}
		if ep.TSNetPort > 0 {
			return fmt.Sprintf("tsnet://%s:%d", host, ep.TSNetPort)
		}
		return "tsnet://" + host
	case "tssvc":
		label := strings.TrimSpace(strings.TrimPrefix(ep.TSServiceName, "svc:"))
		if label == "" {
			label = "cleanroom"
		}
		if ep.TSServicePort > 0 {
			return fmt.Sprintf("tssvc://%s:%d", label, ep.TSServicePort)
		}
		return "tssvc://" + label
	default:
		if ep.Address != "" {
			return ep.Address
		}
		return ep.BaseURL
	}
}

func effectiveLogLevel(rawLevel string) string {
	level := strings.TrimSpace(strings.ToLower(rawLevel))
	if level == "" {
		return "info"
	}
	return level
}

func noColorRequested() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	return strings.TrimSpace(os.Getenv("CLICOLOR")) == "0"
}

func forceColorRequested() bool {
	value := strings.TrimSpace(os.Getenv("CLICOLOR_FORCE"))
	if value == "" {
		return false
	}
	if parsed, err := strconv.Atoi(value); err == nil {
		return parsed != 0
	}
	return true
}

func ansiWrap(code, value string) string {
	return "\x1b[" + code + "m" + value + "\x1b[0m"
}

func normalizeDoctorStatus(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "pass", "ok", "success":
		return "pass"
	case "warn", "warning":
		return "warn"
	case "fail", "failed", "error":
		return "fail"
	default:
		return "unknown"
	}
}

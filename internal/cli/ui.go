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

type daemonStatusReport struct {
	Manager   string
	Service   string
	Installed bool
	Active    bool
	Fields    []startupField
}

type summaryBlock struct {
	Title      string
	TitleStyle terminalStyle
	Fields     []startupField
}

type statusCheckLine struct {
	Name    string
	Status  string
	Message string
}

type terminalStyle struct {
	foregroundCode string
	bold           bool
	faint          bool
}

func (s terminalStyle) wrap(value string, enabled bool) string {
	if !enabled || value == "" {
		return value
	}
	code := s.ansiCode()
	if code == "" {
		return value
	}
	return ansiWrap(code, value)
}

func (s terminalStyle) ansiCode() string {
	parts := make([]string, 0, 3)
	if s.bold {
		parts = append(parts, "1")
	}
	if s.faint {
		parts = append(parts, "2")
	}
	if s.foregroundCode != "" {
		parts = append(parts, s.foregroundCode)
	}
	return strings.Join(parts, ";")
}

func terminalStyleFromLipgloss(style lipgloss.Style) terminalStyle {
	return terminalStyle{
		foregroundCode: terminalForegroundCode(style.GetForeground()),
		bold:           style.GetBold(),
		faint:          style.GetFaint(),
	}
}

type terminalPalette struct {
	icon      terminalStyle
	title     terminalStyle
	text      terminalStyle
	value     terminalStyle
	muted     terminalStyle
	separator terminalStyle
	key       terminalStyle
	debug     terminalStyle
	info      terminalStyle
	warn      terminalStyle
	error     terminalStyle
}

func defaultTerminalPalette() terminalPalette {
	styles := log.DefaultStyles()
	return terminalPalette{
		icon:      terminalStyleFromLipgloss(styles.Levels[log.InfoLevel]),
		title:     terminalStyleFromLipgloss(styles.Levels[log.InfoLevel]),
		text:      terminalStyleFromLipgloss(styles.Message),
		value:     terminalStyleFromLipgloss(styles.Value),
		muted:     terminalStyleFromLipgloss(styles.Separator),
		separator: terminalStyleFromLipgloss(styles.Separator),
		key:       terminalStyleFromLipgloss(styles.Key),
		debug:     terminalStyleFromLipgloss(styles.Levels[log.DebugLevel]),
		info:      terminalStyleFromLipgloss(styles.Levels[log.InfoLevel]),
		warn:      terminalStyleFromLipgloss(styles.Levels[log.WarnLevel]),
		error:     terminalStyleFromLipgloss(styles.Levels[log.ErrorLevel]),
	}
}

func terminalForegroundCode(color lipgloss.TerminalColor) string {
	switch c := color.(type) {
	case lipgloss.NoColor:
		return ""
	case lipgloss.Color:
		return terminalForegroundCodeFromString(string(c))
	case lipgloss.ANSIColor:
		return "38;5;" + strconv.FormatUint(uint64(c), 10)
	case lipgloss.CompleteColor:
		if code := terminalForegroundCodeFromString(c.ANSI256); code != "" {
			return code
		}
		if code := terminalForegroundCodeFromString(c.TrueColor); code != "" {
			return code
		}
		return terminalForegroundCodeFromString(c.ANSI)
	case lipgloss.AdaptiveColor:
		if code := terminalForegroundCodeFromString(c.Dark); code != "" {
			return code
		}
		return terminalForegroundCodeFromString(c.Light)
	case lipgloss.CompleteAdaptiveColor:
		if code := terminalForegroundCode(lipgloss.CompleteColor(c.Dark)); code != "" {
			return code
		}
		return terminalForegroundCode(lipgloss.CompleteColor(c.Light))
	default:
		return ""
	}
}

func terminalForegroundCodeFromString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "#") {
		r, g, b, ok := parseHexColor(value)
		if !ok {
			return ""
		}
		return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
	}
	if _, err := strconv.Atoi(value); err == nil {
		return "38;5;" + value
	}
	return ""
}

func parseHexColor(value string) (int, int, int, bool) {
	trimmed := strings.TrimPrefix(value, "#")
	switch len(trimmed) {
	case 3:
		r, err := strconv.ParseUint(strings.Repeat(string(trimmed[0]), 2), 16, 8)
		if err != nil {
			return 0, 0, 0, false
		}
		g, err := strconv.ParseUint(strings.Repeat(string(trimmed[1]), 2), 16, 8)
		if err != nil {
			return 0, 0, 0, false
		}
		b, err := strconv.ParseUint(strings.Repeat(string(trimmed[2]), 2), 16, 8)
		if err != nil {
			return 0, 0, 0, false
		}
		return int(r), int(g), int(b), true
	case 6:
		r, err := strconv.ParseUint(trimmed[0:2], 16, 8)
		if err != nil {
			return 0, 0, 0, false
		}
		g, err := strconv.ParseUint(trimmed[2:4], 16, 8)
		if err != nil {
			return 0, 0, 0, false
		}
		b, err := strconv.ParseUint(trimmed[4:6], 16, 8)
		if err != nil {
			return 0, 0, 0, false
		}
		return int(r), int(g), int(b), true
	default:
		return 0, 0, 0, false
	}
}

func renderStyledFieldLine(
	indent, key, separator, value string,
	pad bool,
	color bool,
	keyStyle, separatorStyle, valueStyle terminalStyle,
) string {
	spacer := ""
	if pad {
		spacer = " "
	}
	if !color {
		return indent + key + separator + spacer + value
	}
	return indent +
		keyStyle.wrap(key, true) +
		separatorStyle.wrap(separator, true) +
		spacer +
		valueStyle.wrap(value, true)
}

func renderKeyValueLine(indent, key, value string, color bool, palette terminalPalette) string {
	return renderStyledFieldLine(indent, key, ":", value, true, color, palette.key, palette.separator, palette.value)
}

func renderAssignmentLine(indent, key, value string, color bool, palette terminalPalette) string {
	return renderStyledFieldLine(indent, key, "=", value, false, color, palette.key, palette.separator, palette.value)
}

func renderStatusValueLine(label, value string, labelStyle terminalStyle, color bool) string {
	palette := defaultTerminalPalette()
	return renderStyledFieldLine("", label, ":", value, true, color, labelStyle, palette.separator, palette.value)
}

func renderNoticeLine(prefix, message string, prefixStyle terminalStyle, color bool) string {
	palette := defaultTerminalPalette()
	return renderStyledFieldLine("", prefix, ":", message, true, color, prefixStyle, prefixStyle, palette.text) + "\n"
}

func renderActionLine(action, message string, actionStyle terminalStyle, color bool) string {
	palette := defaultTerminalPalette()
	if !color {
		return action + " " + message
	}
	return actionStyle.wrap(action, true) + " " + palette.text.wrap(message, true)
}

func renderSummaryBlock(block summaryBlock, color bool) string {
	palette := defaultTerminalPalette()
	title := strings.TrimSpace(block.Title)

	var out strings.Builder
	if title != "" {
		if color {
			title = block.TitleStyle.wrap(title, true)
		}
		out.WriteString(title)
		out.WriteByte('\n')
	}

	for _, field := range block.Fields {
		key := strings.TrimSpace(field.Key)
		value := strings.TrimSpace(field.Value)
		if key == "" || value == "" {
			continue
		}

		out.WriteString(renderAssignmentLine("", key, value, color, palette))
		out.WriteByte('\n')
	}

	return out.String()
}

func doctorStatusStyle(status string, palette terminalPalette) terminalStyle {
	switch status {
	case "pass":
		return palette.info
	case "warn":
		return palette.warn
	case "fail":
		return palette.error
	default:
		return palette.text
	}
}

func daemonSummaryStyle(report daemonStatusReport, palette terminalPalette) terminalStyle {
	if report.Active {
		return palette.info
	}
	return palette.warn
}

func renderStartupHeader(h startupHeader, color bool) string {
	palette := defaultTerminalPalette()
	title := strings.TrimSpace(h.Title)
	if title == "" {
		title = "cleanroom"
	}

	var out strings.Builder
	icon := "🧑‍🔬"
	if color {
		icon = palette.icon.wrap(icon, true)
		title = palette.title.wrap(title, true)
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

		out.WriteString(renderKeyValueLine("   ", key, value, color, palette))
		out.WriteByte('\n')
	}
	out.WriteByte('\n')

	return out.String()
}

func renderDoctorReport(backendName string, checks []backend.DoctorCheck, color bool) string {
	palette := defaultTerminalPalette()
	name := strings.TrimSpace(backendName)
	if name == "" {
		name = "unknown"
	}

	var out strings.Builder
	title := fmt.Sprintf("doctor report (%s)", name)
	if color {
		title = palette.title.wrap(title, true)
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
			statusBlock = doctorStatusStyle(status, palette).wrap(statusBlock, true)
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
		if color {
			out.WriteString(palette.key.wrap(checkName, true))
			out.WriteString(palette.separator.wrap(":", true))
			out.WriteString(" ")
			out.WriteString(palette.text.wrap(message, true))
		} else {
			out.WriteString(checkName)
			out.WriteString(": ")
			out.WriteString(message)
		}
		out.WriteByte('\n')
	}

	summary := fmt.Sprintf("summary: %d pass, %d warn, %d fail", passCount, warnCount, failCount)
	if color {
		summary = palette.muted.wrap(summary, true)
	}
	out.WriteString(summary)
	out.WriteByte('\n')

	return out.String()
}

func renderStatusCheckReport(title string, checks []statusCheckLine, color bool) string {
	palette := defaultTerminalPalette()
	trimmedTitle := strings.TrimSpace(title)
	if trimmedTitle == "" {
		trimmedTitle = "status"
	}

	var out strings.Builder
	if color {
		out.WriteString(palette.title.wrap(trimmedTitle, true))
	} else {
		out.WriteString(trimmedTitle)
	}
	out.WriteByte('\n')

	for _, check := range checks {
		out.WriteString(renderStatusCheckLine(check, color))
		out.WriteByte('\n')
	}

	return out.String()
}

func renderStatusCheckLine(check statusCheckLine, color bool) string {
	palette := defaultTerminalPalette()
	status := normalizeDoctorStatus(check.Status)
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
		statusBlock = doctorStatusStyle(status, palette).wrap(statusBlock, true)
	}

	name := strings.TrimSpace(check.Name)
	if name == "" {
		name = "status"
	}
	message := strings.TrimSpace(check.Message)
	if message == "" {
		message = "(no message)"
	}

	var out strings.Builder
	out.WriteString(statusBlock)
	out.WriteString(" ")
	if color {
		out.WriteString(palette.key.wrap(name, true))
		out.WriteString(palette.separator.wrap(":", true))
		out.WriteString(" ")
		out.WriteString(palette.text.wrap(message, true))
	} else {
		out.WriteString(name)
		out.WriteString(": ")
		out.WriteString(message)
	}
	return out.String()
}

func renderDaemonStatusReport(report daemonStatusReport, color bool) string {
	palette := defaultTerminalPalette()
	manager := strings.TrimSpace(report.Manager)
	if manager == "" {
		manager = "unknown"
	}

	service := strings.TrimSpace(report.Service)
	if service == "" {
		service = "unknown"
	}

	summary := "not installed"
	icon := "!"
	switch {
	case report.Active:
		summary = "running"
		icon = "✓"
	case report.Installed:
		summary = "installed"
	}

	title := fmt.Sprintf("daemon status (%s)", manager)
	statusBlock := fmt.Sprintf("%s [%s]", icon, summary)
	statusLine := statusBlock + " " + service
	if color {
		title = palette.title.wrap(title, true)
		statusLine = daemonSummaryStyle(report, palette).wrap(statusBlock, true) + " " + palette.text.wrap(service, true)
	}

	var out strings.Builder
	out.WriteString(title)
	out.WriteByte('\n')
	out.WriteString(statusLine)
	out.WriteByte('\n')

	for _, field := range report.Fields {
		key := strings.TrimSpace(field.Key)
		value := strings.TrimSpace(field.Value)
		if key == "" || value == "" {
			continue
		}

		out.WriteString(renderKeyValueLine("  ", key, value, color, palette))
		out.WriteByte('\n')
	}

	return out.String()
}

func daemonInstalledLabel(installed bool) string {
	if installed {
		return "installed"
	}
	return "missing"
}

func daemonRuntimeLabel(active bool) string {
	if active {
		return "active"
	}
	return "inactive"
}

func daemonEnabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
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

func shouldUseANSI(w io.Writer) bool {
	if forceColorRequested() {
		return true
	}
	if noColorRequested() {
		return false
	}
	file, ok := w.(*os.File)
	if !ok || file == nil {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
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

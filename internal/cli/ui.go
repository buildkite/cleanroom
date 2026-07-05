package cli

import (
	"fmt"
	stdcolor "image/color"
	"io"
	"os"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

var isTerminalFunc = term.IsTerminal

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
	text      terminalStyle
	value     terminalStyle
	separator terminalStyle
	key       terminalStyle
	info      terminalStyle
}

func defaultTerminalPalette() terminalPalette {
	info := terminalStyleFromLipgloss(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86")))
	return terminalPalette{
		text:      terminalStyleFromLipgloss(lipgloss.NewStyle()),
		value:     terminalStyleFromLipgloss(lipgloss.NewStyle()),
		separator: terminalStyleFromLipgloss(lipgloss.NewStyle().Faint(true)),
		key:       terminalStyleFromLipgloss(lipgloss.NewStyle().Faint(true)),
		info:      info,
	}
}

func terminalForegroundCode(color stdcolor.Color) string {
	switch c := color.(type) {
	case nil:
		return ""
	case lipgloss.NoColor:
		return ""
	case ansi.BasicColor:
		return "38;5;" + strconv.FormatUint(uint64(c), 10)
	case ansi.IndexedColor:
		return "38;5;" + strconv.FormatUint(uint64(c), 10)
	case lipgloss.RGBColor:
		return fmt.Sprintf("38;2;%d;%d;%d", c.R, c.G, c.B)
	case stdcolor.RGBA:
		if c.A == 0 {
			return ""
		}
		return fmt.Sprintf("38;2;%d;%d;%d", c.R, c.G, c.B)
	case stdcolor.NRGBA:
		if c.A == 0 {
			return ""
		}
		return fmt.Sprintf("38;2;%d;%d;%d", c.R, c.G, c.B)
	default:
		r, g, b, a := c.RGBA()
		if a == 0 {
			return ""
		}
		return fmt.Sprintf("38;2;%d;%d;%d", uint8(r>>8), uint8(g>>8), uint8(b>>8))
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

func renderStatusValueLine(label, value string, labelStyle terminalStyle, color bool) string {
	palette := defaultTerminalPalette()
	return renderStyledFieldLine("", label, ":", value, true, color, labelStyle, palette.separator, palette.value)
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
	return isTerminalFunc(int(file.Fd()))
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

package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	sandboxProgressStartDelay   = 250 * time.Millisecond
	sandboxProgressTickInterval = 100 * time.Millisecond
)

const defaultSandboxProgressMessage = "Preparing sandbox (first use may take a bit)..."

var sandboxProgressFrames = []string{"⣾ ", "⣽ ", "⣻ ", "⢿ ", "⡿ ", "⣟ ", "⣯ ", "⣷ "}

type sandboxProgress struct {
	mu         sync.Mutex
	stderr     *os.File
	useANSI    bool
	shown      bool
	suppressed bool
	message    string
}

func (p *sandboxProgress) renderStart() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suppressed {
		return false
	}
	p.shown = true
	if p.stderr != nil {
		writeSandboxProgressStart(p.stderr, p.messageLocked())
	}
	return true
}

func (p *sandboxProgress) renderFrame(frame string, elapsed time.Duration) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suppressed {
		return false
	}
	p.shown = true
	if p.stderr != nil {
		writeSandboxProgressFrameMessage(p.stderr, frame, p.messageLocked(), elapsed)
	}
	return true
}

func (p *sandboxProgress) setMessage(message string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suppressed {
		return
	}
	message = sanitizeSandboxProgressMessage(message)
	if message == "" || message == p.message {
		return
	}
	p.message = message
	if p.shown && !p.useANSI && p.stderr != nil {
		writeSandboxProgressStart(p.stderr, p.messageLocked())
	}
}

func (p *sandboxProgress) suppress() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suppressed {
		return
	}
	p.suppressed = true
	if p.shown && p.useANSI && p.stderr != nil {
		_, _ = fmt.Fprint(p.stderr, "\r\033[2K")
	}
}

func (p *sandboxProgress) wasShown() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shown
}

func (p *sandboxProgress) messageLocked() string {
	if p == nil || p.message == "" {
		return defaultSandboxProgressMessage
	}
	return p.message
}

func (p *sandboxProgress) complete(success bool, elapsed time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if (!p.shown && !p.suppressed) || p.stderr == nil {
		return
	}
	if success && p.suppressed {
		return
	}
	if p.useANSI {
		writeSandboxProgressComplete(p.stderr, success, elapsed)
		return
	}
	writeSandboxProgressCompletePlain(p.stderr, success, elapsed)
}

func withSandboxProgress(stderr *os.File, fn func(progress *sandboxProgress) error) error {
	if stderr == nil || !isTerminalFunc(int(stderr.Fd())) {
		return fn(nil)
	}

	useANSI := shouldUseANSI(stderr)
	startedAt := time.Now()
	done := make(chan struct{})
	stopped := make(chan struct{})
	progress := &sandboxProgress{stderr: stderr, useANSI: useANSI}

	go func() {
		defer close(stopped)

		timer := time.NewTimer(sandboxProgressStartDelay)
		defer timer.Stop()

		select {
		case <-done:
			return
		case <-timer.C:
		}

		if !useANSI {
			progress.renderStart()
			<-done
			return
		}

		ticker := time.NewTicker(sandboxProgressTickInterval)
		defer ticker.Stop()

		frame := 0
		progress.renderFrame(sandboxProgressFrames[frame], time.Since(startedAt))
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				frame = (frame + 1) % len(sandboxProgressFrames)
				progress.renderFrame(sandboxProgressFrames[frame], time.Since(startedAt))
			}
		}
	}()

	err := fn(progress)
	close(done)
	<-stopped

	progress.complete(err == nil, time.Since(startedAt))
	return err
}

func writeSandboxProgressStart(stderr *os.File, messages ...string) {
	if stderr == nil {
		return
	}
	message := defaultSandboxProgressMessage
	if len(messages) > 0 {
		message = messages[0]
	}
	_, _ = fmt.Fprintln(stderr, sanitizeSandboxProgressMessage(message))
}

func writeSandboxProgressFrame(stderr *os.File, frame string, elapsed time.Duration) {
	writeSandboxProgressFrameMessage(stderr, frame, defaultSandboxProgressMessage, elapsed)
}

func writeSandboxProgressFrameMessage(stderr *os.File, frame, message string, elapsed time.Duration) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "\r\033[2K%s %s %s", frame, sanitizeSandboxProgressMessage(message), formatSandboxProgressDuration(elapsed))
}

func writeSandboxProgressComplete(stderr *os.File, success bool, elapsed time.Duration) {
	if stderr == nil {
		return
	}
	if success {
		_, _ = fmt.Fprint(stderr, "\r\033[2K")
		return
	}
	_, _ = fmt.Fprintf(stderr, "\r\033[2KSandbox creation failed in %s\n", formatSandboxProgressDuration(elapsed))
}

func writeSandboxProgressCompletePlain(stderr *os.File, success bool, elapsed time.Duration) {
	if stderr == nil {
		return
	}
	if success {
		return
	}
	_, _ = fmt.Fprintf(stderr, "Sandbox creation failed in %s\n", formatSandboxProgressDuration(elapsed))
}

func formatSandboxProgressDuration(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed < time.Second {
		return elapsed.Round(10 * time.Millisecond).String()
	}
	return elapsed.Round(100 * time.Millisecond).String()
}

func sanitizeSandboxProgressMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return defaultSandboxProgressMessage
	}
	message = strings.ReplaceAll(message, "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	return strings.Join(strings.Fields(message), " ")
}

func sandboxProgressMessage(message string) string {
	message = sanitizeSandboxProgressMessage(message)
	lower := strings.ToLower(message)
	if strings.Contains(lower, "sandbox image") || strings.Contains(lower, "rootfs") {
		return message
	}
	return ""
}

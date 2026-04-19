package cli

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	sandboxProgressStartDelay   = 250 * time.Millisecond
	sandboxProgressTickInterval = 100 * time.Millisecond
)

var sandboxProgressFrames = []string{"-", "\\", "|", "/"}

type sandboxProgress struct {
	mu         sync.Mutex
	stderr     *os.File
	useANSI    bool
	shown      bool
	suppressed bool
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
		writeSandboxProgressStart(p.stderr)
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
		writeSandboxProgressFrame(p.stderr, frame, elapsed)
	}
	return true
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

func (p *sandboxProgress) complete(success bool, elapsed time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.shown || p.stderr == nil {
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

	if progress.wasShown() {
		progress.complete(err == nil, time.Since(startedAt))
	}
	return err
}

func writeSandboxProgressStart(stderr *os.File) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintln(stderr, "Preparing sandbox (first use may take a bit)...")
}

func writeSandboxProgressFrame(stderr *os.File, frame string, elapsed time.Duration) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "\r\033[2K[%s] Preparing sandbox (first use may take a bit)... %s", frame, formatSandboxProgressDuration(elapsed))
}

func writeSandboxProgressComplete(stderr *os.File, success bool, elapsed time.Duration) {
	if stderr == nil {
		return
	}
	message := "Sandbox ready"
	if !success {
		message = "Sandbox creation failed"
	}
	_, _ = fmt.Fprintf(stderr, "\r\033[2K%s in %s\n", message, formatSandboxProgressDuration(elapsed))
}

func writeSandboxProgressCompletePlain(stderr *os.File, success bool, elapsed time.Duration) {
	if stderr == nil {
		return
	}
	message := "Sandbox ready"
	if !success {
		message = "Sandbox creation failed"
	}
	_, _ = fmt.Fprintf(stderr, "%s in %s\n", message, formatSandboxProgressDuration(elapsed))
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

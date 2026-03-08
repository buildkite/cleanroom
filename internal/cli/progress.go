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

type sandboxProgressState struct {
	mu    sync.Mutex
	shown bool
}

func (s *sandboxProgressState) markShown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shown = true
}

func (s *sandboxProgressState) wasShown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shown
}

func withSandboxProgress(stderr *os.File, fn func() error) error {
	if stderr == nil || !isTerminalFunc(int(stderr.Fd())) {
		return fn()
	}

	startedAt := time.Now()
	done := make(chan struct{})
	stopped := make(chan struct{})
	state := &sandboxProgressState{}

	go func() {
		defer close(stopped)

		timer := time.NewTimer(sandboxProgressStartDelay)
		defer timer.Stop()

		select {
		case <-done:
			return
		case <-timer.C:
		}

		state.markShown()
		ticker := time.NewTicker(sandboxProgressTickInterval)
		defer ticker.Stop()

		frame := 0
		writeSandboxProgressFrame(stderr, sandboxProgressFrames[frame], time.Since(startedAt))
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				frame = (frame + 1) % len(sandboxProgressFrames)
				writeSandboxProgressFrame(stderr, sandboxProgressFrames[frame], time.Since(startedAt))
			}
		}
	}()

	err := fn()
	close(done)
	<-stopped

	if state.wasShown() {
		writeSandboxProgressComplete(stderr, err == nil, time.Since(startedAt))
	}
	return err
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

func formatSandboxProgressDuration(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed < time.Second {
		return elapsed.Round(10 * time.Millisecond).String()
	}
	return elapsed.Round(100 * time.Millisecond).String()
}

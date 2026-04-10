package backend

import "sync"

// WarningEmitter deduplicates and dispatches execution warnings. It is
// safe for concurrent use and is shared between the firecracker and
// darwin-vz backends.
type WarningEmitter struct {
	mu      sync.Mutex
	handler func(string)
	seen    map[string]struct{}
}

// SetHandler replaces the warning handler and resets the deduplication set.
func (w *WarningEmitter) SetHandler(handler func(string)) {
	w.mu.Lock()
	w.handler = handler
	w.seen = make(map[string]struct{})
	w.mu.Unlock()
}

// HasHandler reports whether a warning handler is currently set.
func (w *WarningEmitter) HasHandler() bool {
	w.mu.Lock()
	has := w.handler != nil
	w.mu.Unlock()
	return has
}

// Emit sends a warning message to the handler if one is set and the message
// has not already been emitted during this handler's lifetime. The handler
// is called under the lock to prevent races with SetHandler(nil).
func (w *WarningEmitter) Emit(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.handler == nil {
		return
	}
	if _, alreadyWarned := w.seen[msg]; alreadyWarned {
		return
	}
	w.seen[msg] = struct{}{}
	w.handler(msg)
}

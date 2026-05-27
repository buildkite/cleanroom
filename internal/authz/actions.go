package authz

import (
	"errors"
	"fmt"
	"strings"
)

var knownActions = map[string]struct{}{
	"sandbox.create":       {},
	"sandbox.get":          {},
	"sandbox.list":         {},
	"sandbox.terminate":    {},
	"sandbox.file.stat":    {},
	"sandbox.file.walk":    {},
	"sandbox.file.read":    {},
	"sandbox.file.write":   {},
	"sandbox.file.remove":  {},
	"sandbox.file.archive": {},
	"sandbox.file.extract": {},
	"sandbox.port.dial":    {},

	"execution.create":      {},
	"execution.get":         {},
	"execution.list":        {},
	"execution.inspect":     {},
	"execution.cancel":      {},
	"execution.stdin.write": {},
	"execution.stdin.close": {},
	"execution.stream":      {},
	"execution.attach":      {},

	"snapshot.create":  {},
	"snapshot.get":     {},
	"snapshot.list":    {},
	"snapshot.delete":  {},
	"snapshot.restore": {},

	"cache_peer.lookup": {},
	"cache_peer.export": {},
}

var knownResourceKinds = map[string]struct{}{
	"sandbox":    {},
	"execution":  {},
	"snapshot":   {},
	"cache_peer": {},
}

func normalizeActionSet(actions []string) (map[string]struct{}, error) {
	if len(actions) == 0 {
		return nil, errors.New("must contain at least one action")
	}
	out := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		action = strings.TrimSpace(action)
		if action == "" {
			continue
		}
		if _, ok := knownActions[action]; !ok {
			return nil, fmt.Errorf("unknown action %q", action)
		}
		out[action] = struct{}{}
	}
	if len(out) == 0 {
		return nil, errors.New("must contain at least one action")
	}
	return out, nil
}

func normalizeResourceSet(resources []string) (map[string]struct{}, error) {
	if len(resources) == 0 {
		return nil, errors.New("must contain at least one resource")
	}
	out := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		resource = strings.TrimSpace(resource)
		if resource == "" {
			continue
		}
		if _, ok := knownResourceKinds[resource]; !ok {
			return nil, fmt.Errorf("unknown resource %q", resource)
		}
		out[resource] = struct{}{}
	}
	if len(out) == 0 {
		return nil, errors.New("must contain at least one resource")
	}
	return out, nil
}

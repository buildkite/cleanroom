//go:build linux

package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

const guestBootTimingEnv = "CLEANROOM_GUEST_BOOT_TIMINGS"

type guestBootTimingClock func() (int64, error)

type guestBootTimingStore struct {
	mu      sync.Mutex
	timings map[string]int64
	now     guestBootTimingClock
}

var guestBootTimings = newGuestBootTimingStoreFromEnv(os.Getenv(guestBootTimingEnv))

func newGuestBootTimingStoreFromEnv(env string) *guestBootTimingStore {
	if strings.TrimSpace(env) == "" {
		return &guestBootTimingStore{}
	}
	return newGuestBootTimingStore(env, newGuestBootTimingClock())
}

func newGuestBootTimingClock() guestBootTimingClock {
	start := time.Now()
	startMS, err := readGuestBootUptimeMS()
	if err != nil {
		return readGuestBootUptimeMS
	}
	return func() (int64, error) {
		return startMS + time.Since(start).Milliseconds(), nil
	}
}

func newGuestBootTimingStore(env string, now guestBootTimingClock) *guestBootTimingStore {
	store := &guestBootTimingStore{
		timings: parseGuestBootTimingEnv(env),
		now:     now,
	}
	store.record(vsockexec.GuestBootTimingAgentStart)
	return store
}

func newGuestBootTimingStoreFromInitial(initial map[string]int64, now guestBootTimingClock) *guestBootTimingStore {
	timings := map[string]int64{}
	for key, value := range initial {
		if strings.TrimSpace(key) != "" && value >= 0 {
			timings[key] = value
		}
	}
	store := &guestBootTimingStore{
		timings: timings,
		now:     now,
	}
	store.record(vsockexec.GuestBootTimingAgentStart)
	return store
}

func parseGuestBootTimingEnv(env string) map[string]int64 {
	out := map[string]int64{}
	for _, entry := range strings.Split(env, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		ms, err := parseProcUptimeMS(strings.TrimSpace(value))
		if err != nil || ms < 0 {
			continue
		}
		out[key] = ms
	}
	return out
}

func (s *guestBootTimingStore) record(name string) {
	name = strings.TrimSpace(name)
	if s == nil || name == "" || s.now == nil {
		return
	}
	ms, err := s.now()
	if err != nil || ms < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timings == nil {
		s.timings = map[string]int64{}
	}
	s.timings[name] = ms
}

func (s *guestBootTimingStore) recordOnce(name string) {
	name = strings.TrimSpace(name)
	if s == nil || name == "" || s.now == nil {
		return
	}
	s.mu.Lock()
	_, exists := s.timings[name]
	s.mu.Unlock()
	if exists {
		return
	}
	ms, err := s.now()
	if err != nil || ms < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.timings[name]; exists {
		return
	}
	if s.timings == nil {
		s.timings = map[string]int64{}
	}
	s.timings[name] = ms
}

func (s *guestBootTimingStore) snapshot() map[string]int64 {
	if s == nil || s.now == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.timings) == 0 {
		return nil
	}
	out := make(map[string]int64, len(s.timings))
	for key, value := range s.timings {
		out[key] = value
	}
	return out
}

func readGuestBootUptimeMS() (int64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	return parseProcUptimeMS(string(b))
}

func parseProcUptimeMS(raw string) (int64, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0, errors.New("missing uptime")
	}
	secondsRaw, fractionRaw, _ := strings.Cut(fields[0], ".")
	seconds, err := strconv.ParseInt(secondsRaw, 10, 64)
	if err != nil {
		return 0, err
	}
	fractionRaw += "000"
	millis, err := strconv.ParseInt(fractionRaw[:3], 10, 64)
	if err != nil {
		return 0, err
	}
	return seconds*1000 + millis, nil
}

package darwinvz

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	networkFilterStatusPathEnv         = "CLEANROOM_NETWORK_FILTER_STATUS_PATH"
	networkFilterStatusSnapshotVersion = 1
	networkFilterStatusRelativePath    = "Library/Application Support/Cleanroom/network-filter-status.json"
	networkFilterStatusMaxAge          = 10 * time.Minute
)

type networkFilterStatusSnapshot struct {
	Version   int    `json:"version"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Available bool   `json:"available"`
	Loaded    bool   `json:"loaded"`
	Enabled   bool   `json:"enabled"`
	LastError string `json:"last_error,omitempty"`
}

func hostEgressFilterEnabled() (bool, string) {
	snapshot, found, err := readNetworkFilterStatusSnapshot()
	if err != nil {
		return false, err.Error()
	}
	if !found {
		return false, "network filter status file not found"
	}
	if snapshot.Enabled {
		fresh, freshnessDetail := networkFilterStatusFreshness(snapshot.UpdatedAt, time.Now().UTC())
		if !fresh {
			return false, freshnessDetail
		}
		return true, ""
	}
	if lastError := strings.TrimSpace(snapshot.LastError); lastError != "" {
		return false, lastError
	}
	if !snapshot.Available {
		return false, "network filter extension is unavailable"
	}
	if snapshot.Loaded {
		return false, "network filter is disabled"
	}
	return false, "network filter status is not loaded"
}

func readNetworkFilterStatusSnapshot() (networkFilterStatusSnapshot, bool, error) {
	path, err := resolveNetworkFilterStatusPath()
	if err != nil {
		return networkFilterStatusSnapshot{}, false, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return networkFilterStatusSnapshot{}, false, nil
	}
	if err != nil {
		return networkFilterStatusSnapshot{}, false, fmt.Errorf("read network filter status from %s: %w", path, err)
	}
	var snapshot networkFilterStatusSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return networkFilterStatusSnapshot{}, false, fmt.Errorf("parse network filter status from %s: %w", path, err)
	}
	if snapshot.Version != 0 && snapshot.Version != networkFilterStatusSnapshotVersion {
		return networkFilterStatusSnapshot{}, false, fmt.Errorf(
			"network filter status version %d is unsupported (expected %d)",
			snapshot.Version,
			networkFilterStatusSnapshotVersion,
		)
	}
	return snapshot, true, nil
}

func networkFilterStatusFreshness(updatedAt string, now time.Time) (bool, string) {
	timestamp := strings.TrimSpace(updatedAt)
	if timestamp == "" {
		return false, "network filter status timestamp is missing"
	}

	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, timestamp)
	}
	if err != nil {
		return false, fmt.Sprintf("network filter status timestamp %q is invalid", timestamp)
	}
	if parsed.After(now.Add(2 * time.Minute)) {
		return false, fmt.Sprintf("network filter status timestamp %q is in the future", timestamp)
	}
	age := now.Sub(parsed)
	if age > networkFilterStatusMaxAge {
		return false, fmt.Sprintf("network filter status is stale (last update %s ago)", age.Round(time.Second))
	}
	return true, ""
}

func resolveNetworkFilterStatusPath() (string, error) {
	if configured := strings.TrimSpace(os.Getenv(networkFilterStatusPathEnv)); configured != "" {
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory for network filter status: %w", err)
	}
	return filepath.Join(home, networkFilterStatusRelativePath), nil
}

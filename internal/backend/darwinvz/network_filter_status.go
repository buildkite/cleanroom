package darwinvz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/networkfilterstate"
)

const (
	networkFilterStatusPathEnv         = "CLEANROOM_NETWORK_FILTER_STATUS_PATH"
	networkFilterDaemonURLEnv          = "CLEANROOM_NETWORK_FILTER_DAEMON_URL"
	networkFilterStatusSnapshotVersion = 1
	networkFilterStatusMaxAge          = 10 * time.Minute
)

type networkFilterStatusSnapshot struct {
	Version           int    `json:"version"`
	UpdatedAt         string `json:"updated_at,omitempty"`
	Available         bool   `json:"available"`
	Loaded            bool   `json:"loaded"`
	Enabled           bool   `json:"enabled"`
	LastError         string `json:"last_error,omitempty"`
	ProviderStartedAt string `json:"provider_started_at,omitempty"`
	ProviderLastError string `json:"provider_last_error,omitempty"`
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
		if providerLastError := strings.TrimSpace(snapshot.ProviderLastError); providerLastError != "" {
			return false, providerLastError
		}
		fresh, freshnessDetail := networkFilterStatusFreshness(snapshot.UpdatedAt, time.Now().UTC())
		if !fresh {
			return false, freshnessDetail
		}
		if strings.TrimSpace(snapshot.ProviderStartedAt) == "" {
			return false, "network filter provider has not started"
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
	if strings.TrimSpace(os.Getenv(networkFilterStatusPathEnv)) == "" {
		snapshot, found, err := readNetworkFilterStatusSnapshotFromDaemon()
		if err == nil || found {
			return snapshot, found, err
		}
	}

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

func readNetworkFilterStatusSnapshotFromDaemon() (networkFilterStatusSnapshot, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	snapshot, found, err := networkfilterstate.NewClient(resolveNetworkFilterDaemonURL()).GetStatus(ctx)
	if err != nil {
		return networkFilterStatusSnapshot{}, false, err
	}
	if !found {
		return networkFilterStatusSnapshot{}, false, nil
	}
	return networkFilterStatusSnapshot{
		Version:           snapshot.Version,
		UpdatedAt:         snapshot.UpdatedAt,
		Available:         snapshot.Available,
		Loaded:            snapshot.Loaded,
		Enabled:           snapshot.Enabled,
		LastError:         snapshot.LastError,
		ProviderStartedAt: snapshot.ProviderStartedAt,
		ProviderLastError: snapshot.ProviderLastError,
	}, true, nil
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
	return "", fmt.Errorf("%s is not set and the network-filter daemon did not return status", networkFilterStatusPathEnv)
}

func resolveNetworkFilterDaemonURL() string {
	if configured := strings.TrimSpace(os.Getenv(networkFilterDaemonURLEnv)); configured != "" {
		return configured
	}
	return networkfilterstate.DefaultBaseURL
}

package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func detectInstalledGatewayCredentialHosts() installedGatewayCredentialHostsResult {
	switch serveInstallGOOS {
	case "darwin":
		path, err := launchdUserServicePath()
		if err != nil {
			return installedGatewayCredentialHostsResult{Err: err}
		}
		return installedGatewayCredentialHostsFromFile(path, launchdProgramArguments)
	case "linux":
		return installedGatewayCredentialHostsFromFile(serveInstallSystemdUnitPath, systemdProgramArguments)
	default:
		return installedGatewayCredentialHostsResult{}
	}
}

func installedGatewayCredentialHostsFromFile(path string, parseArgs func([]byte) ([]string, error)) installedGatewayCredentialHostsResult {
	if _, err := serveInstallStat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return installedGatewayCredentialHostsResult{}
		}
		return installedGatewayCredentialHostsResult{Err: fmt.Errorf("stat daemon service file: %w", err)}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return installedGatewayCredentialHostsResult{Installed: true, Err: fmt.Errorf("read daemon service file: %w", err)}
	}
	args, err := parseArgs(raw)
	if err != nil {
		return installedGatewayCredentialHostsResult{Installed: true, Err: fmt.Errorf("parse daemon service arguments: %w", err)}
	}
	hosts, err := gatewayCredentialHostsFromServeArgs(args)
	if err != nil {
		return installedGatewayCredentialHostsResult{Installed: true, Err: err}
	}
	return installedGatewayCredentialHostsResult{Hosts: hosts, Installed: true}
}

func gatewayCredentialHostsFromServeArgs(args []string) ([]string, error) {
	cfg := runtimeconfig.GatewayGitHubAppCredentialsConfig{}
	for i := 0; i < len(args); i++ {
		switch strings.TrimSpace(args[i]) {
		case "--github-app-id":
			if i+1 < len(args) {
				cfg.AppID = runtimeconfig.ScalarString(strings.TrimSpace(args[i+1]))
				i++
			}
		case "--github-app-installation-id":
			if i+1 < len(args) {
				cfg.InstallationID = runtimeconfig.ScalarString(strings.TrimSpace(args[i+1]))
				i++
			}
		case "--github-app-private-key-file":
			if i+1 < len(args) {
				cfg.PrivateKeyFile = strings.TrimSpace(args[i+1])
				i++
			}
		case "--github-app-repo-prefixes":
			if i+1 < len(args) {
				cfg.RepoPrefixes = append(cfg.RepoPrefixes, splitCredentialHostPrefixes(args[i+1])...)
				i++
			}
		}
	}

	cfg.RepoPrefixes = trimNonEmptyStrings(cfg.RepoPrefixes)
	if !runtimeconfig.GatewayGitHubAppCredentialsConfigured(cfg) {
		return nil, nil
	}
	if strings.TrimSpace(string(cfg.AppID)) == "" ||
		strings.TrimSpace(string(cfg.InstallationID)) == "" ||
		strings.TrimSpace(cfg.PrivateKeyFile) == "" ||
		len(cfg.RepoPrefixes) == 0 {
		return nil, errors.New("installed daemon GitHub App credential arguments are incomplete")
	}
	return []string{"github.com"}, nil
}

func splitCredentialHostPrefixes(raw string) []string {
	return trimNonEmptyStrings(strings.Split(raw, ","))
}

func systemdProgramArguments(unitContent []byte) ([]string, error) {
	for _, line := range strings.Split(string(unitContent), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "ExecStart" {
			continue
		}
		return splitSystemdExecArgs(strings.TrimSpace(value))
	}
	return nil, errors.New("ExecStart not found")
}

func splitSystemdExecArgs(value string) ([]string, error) {
	var args []string
	for {
		value = strings.TrimLeft(value, " \t")
		if value == "" {
			return args, nil
		}

		if value[0] == '"' {
			token, rest, err := consumeQuotedSystemdArg(value)
			if err != nil {
				return nil, err
			}
			unquoted, err := strconv.Unquote(token)
			if err != nil {
				return nil, fmt.Errorf("parse quoted ExecStart argument: %w", err)
			}
			args = append(args, unquoted)
			value = rest
			continue
		}

		end := strings.IndexAny(value, " \t")
		if end < 0 {
			args = append(args, value)
			return args, nil
		}
		args = append(args, value[:end])
		value = value[end:]
	}
}

func consumeQuotedSystemdArg(value string) (string, string, error) {
	escaped := false
	for i := 1; i < len(value); i++ {
		ch := value[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			return value[:i+1], value[i+1:], nil
		}
	}
	return "", "", errors.New("unterminated quoted ExecStart argument")
}

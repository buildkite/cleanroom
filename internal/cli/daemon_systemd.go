package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func uninstallSystemdDaemon(stdout io.Writer) error {
	if _, err := serveInstallStat(serveInstallSystemdUnitPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service file %s does not exist", serveInstallSystemdUnitPath)
	}

	if err := serveInstallRunCommand("systemctl", "stop", systemdServiceName); err != nil {
		return fmt.Errorf("stop systemd service %s: %w", systemdServiceName, err)
	}
	if err := serveInstallRunCommand("systemctl", "disable", systemdServiceName); err != nil {
		return fmt.Errorf("disable systemd service %s: %w", systemdServiceName, err)
	}

	if err := serveInstallRemoveFile(serveInstallSystemdUnitPath); err != nil {
		return fmt.Errorf("remove service file %s: %w", serveInstallSystemdUnitPath, err)
	}
	if err := serveInstallRunCommand("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd: %w", err)
	}

	_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon uninstalled",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "systemd"},
			{Key: "service", Value: systemdServiceName},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func startSystemdDaemon(stdout io.Writer) error {
	if _, err := serveInstallStat(serveInstallSystemdUnitPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service file %s does not exist", serveInstallSystemdUnitPath)
	}

	if err := serveInstallRunCommand("systemctl", "start", systemdServiceName); err != nil {
		return fmt.Errorf("start systemd service %s: %w", systemdServiceName, err)
	}

	_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon started",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "systemd"},
			{Key: "service", Value: systemdServiceName},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func stopSystemdDaemon(stdout io.Writer) error {
	if _, err := serveInstallStat(serveInstallSystemdUnitPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service file %s does not exist", serveInstallSystemdUnitPath)
	}

	if err := serveInstallRunCommand("systemctl", "stop", systemdServiceName); err != nil {
		return fmt.Errorf("stop systemd service %s: %w", systemdServiceName, err)
	}

	_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon stopped",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "systemd"},
			{Key: "service", Value: systemdServiceName},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func systemdDaemonStatus(stdout *os.File) error {
	installed := false
	if _, err := serveInstallStat(serveInstallSystemdUnitPath); err == nil {
		installed = true
	}

	active := false
	if err := serveInstallRunCommand("systemctl", "is-active", "--quiet", systemdServiceName); err == nil {
		active = true
	} else if !isExitError(err) {
		return fmt.Errorf("check systemd service active state: %w", err)
	}

	enabled := false
	if err := serveInstallRunCommand("systemctl", "is-enabled", "--quiet", systemdServiceName); err == nil {
		enabled = true
	} else if !isExitError(err) {
		return fmt.Errorf("check systemd service enabled state: %w", err)
	}

	_, err := fmt.Fprint(stdout, renderDaemonStatusReport(daemonStatusReport{
		Manager:   "systemd",
		Service:   systemdServiceName,
		Installed: installed,
		Active:    active,
		Fields: []startupField{
			{Key: "install", Value: daemonInstalledLabel(installed)},
			{Key: "runtime", Value: daemonRuntimeLabel(active)},
			{Key: "enabled", Value: daemonEnabledLabel(enabled)},
			{Key: "path", Value: serveInstallSystemdUnitPath},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func installSystemdDaemon(stdout io.Writer, executablePath string, args []string, force bool) error {
	content := renderSystemdService(executablePath, args)
	if err := writeDaemonFile(serveInstallSystemdUnitPath, content, force, 0o644); err != nil {
		return err
	}

	if err := serveInstallRunCommand("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd: %w", err)
	}
	if err := serveInstallRunCommand("systemctl", "enable", "--now", systemdServiceName); err != nil {
		return fmt.Errorf("enable systemd service %s: %w", systemdServiceName, err)
	}
	if force {
		if err := serveInstallRunCommand("systemctl", "restart", systemdServiceName); err != nil {
			return fmt.Errorf("restart systemd service %s: %w", systemdServiceName, err)
		}
	}

	_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon installed",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "systemd"},
			{Key: "service", Value: systemdServiceName},
			{Key: "path", Value: serveInstallSystemdUnitPath},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func renderSystemdService(executablePath string, args []string) string {
	cmd := append([]string{executablePath}, args...)
	return fmt.Sprintf(`[Unit]
Description=Cleanroom control-plane server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, joinSystemdExecArgs(cmd))
}

func joinSystemdExecArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\"\\'") {
			quoted = append(quoted, strconv.Quote(arg))
			continue
		}
		quoted = append(quoted, arg)
	}
	return strings.Join(quoted, " ")
}

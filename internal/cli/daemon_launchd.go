package cli

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func uninstallLaunchdDaemon(stdout io.Writer) error {
	return uninstallLaunchdDaemonInDomain(stdout, serveInstallLaunchdPath, "system")
}

func uninstallLaunchdUserDaemon(stdout io.Writer) error {
	path, err := launchdUserServicePath()
	if err != nil {
		return err
	}
	return uninstallLaunchdDaemonInDomain(stdout, path, launchdUserDomain())
}

func uninstallLaunchdDaemonInDomain(stdout io.Writer, servicePath, domain string) error {
	serviceFileExists := false
	if _, err := serveInstallStat(servicePath); err == nil {
		serviceFileExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat service file %s: %w", servicePath, err)
	}

	bootoutFoundService := true
	if err := serveInstallRunCommand("launchctl", "bootout", launchdServiceTarget(domain)); err != nil {
		if !isExitError(err) {
			return fmt.Errorf("bootout launchd service %s: %w", launchdServiceName, err)
		}
		bootoutFoundService = false
	}

	if serviceFileExists {
		if err := serveInstallRemoveFile(servicePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove service file %s: %w", servicePath, err)
		}
	}

	message := "daemon uninstalled"
	if !serviceFileExists && !bootoutFoundService {
		message = "daemon already uninstalled"
	}
	titleStyle := defaultTerminalPalette().info
	if strings.Contains(message, "already") {
		titleStyle = defaultTerminalPalette().warn
	}
	_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      message,
		TitleStyle: titleStyle,
		Fields: []startupField{
			{Key: "manager", Value: "launchd"},
			{Key: "service", Value: launchdServiceName},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func startLaunchdDaemon(stdout io.Writer) error {
	return startLaunchdDaemonInDomain(stdout, serveInstallLaunchdPath, "system")
}

func startLaunchdUserDaemon(stdout io.Writer) error {
	path, err := launchdUserServicePath()
	if err != nil {
		return err
	}
	return startLaunchdDaemonInDomain(stdout, path, launchdUserDomain())
}

func startLaunchdDaemonInDomain(stdout io.Writer, servicePath, domain string) error {
	if _, err := serveInstallStat(servicePath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service file %s does not exist", servicePath)
	} else if err != nil {
		return fmt.Errorf("stat service file %s: %w", servicePath, err)
	}

	target := launchdServiceTarget(domain)
	if err := serveInstallRunCommand("launchctl", "enable", target); err != nil {
		return fmt.Errorf("enable launchd service %s: %w", launchdServiceName, err)
	}
	loaded, err := launchdServiceLoaded(target)
	if err != nil {
		return err
	}
	if !loaded {
		if err := serveInstallRunCommand("launchctl", "bootstrap", domain, servicePath); err != nil {
			return fmt.Errorf("bootstrap launchd service %s: %w", launchdServiceName, err)
		}
	}
	if err := serveInstallRunCommand("launchctl", "kickstart", "-k", target); err != nil {
		return fmt.Errorf("start launchd service %s: %w", launchdServiceName, err)
	}

	_, err = fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon started",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "launchd"},
			{Key: "service", Value: launchdServiceName},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func stopLaunchdDaemon(stdout io.Writer) error {
	return stopLaunchdDaemonInDomain(stdout, serveInstallLaunchdPath, "system")
}

func stopLaunchdUserDaemon(stdout io.Writer) error {
	path, err := launchdUserServicePath()
	if err != nil {
		return err
	}
	return stopLaunchdDaemonInDomain(stdout, path, launchdUserDomain())
}

func stopLaunchdDaemonInDomain(stdout io.Writer, servicePath, domain string) error {
	serviceFileExists := false
	statErr := error(nil)
	if _, err := serveInstallStat(servicePath); err == nil {
		serviceFileExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		statErr = err
	}
	if statErr != nil {
		return fmt.Errorf("stat service file %s: %w", servicePath, statErr)
	}

	target := launchdServiceTarget(domain)
	loaded, err := launchdServiceLoaded(target)
	if err != nil {
		return err
	}
	if !serviceFileExists && !loaded {
		_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
			Title:      "daemon already stopped",
			TitleStyle: defaultTerminalPalette().warn,
			Fields: []startupField{
				{Key: "manager", Value: "launchd"},
				{Key: "service", Value: launchdServiceName},
			},
		}, shouldUseANSI(stdout)))
		return err
	}

	if err := serveInstallRunCommand("launchctl", "disable", target); err != nil {
		return fmt.Errorf("disable launchd service %s: %w", launchdServiceName, err)
	}
	if loaded {
		if err := serveInstallRunCommand("launchctl", "bootout", target); err != nil {
			return fmt.Errorf("stop launchd service %s: %w", launchdServiceName, err)
		}
	}

	_, err = fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon stopped",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "launchd"},
			{Key: "service", Value: launchdServiceName},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func restartLaunchdDaemon(stdout io.Writer, force bool) error {
	return restartLaunchdDaemonInDomain(stdout, serveInstallLaunchdPath, "system", force)
}

func restartLaunchdUserDaemon(stdout io.Writer, force bool) error {
	path, err := launchdUserServicePath()
	if err != nil {
		return err
	}
	return restartLaunchdDaemonInDomain(stdout, path, launchdUserDomain(), force)
}

func restartLaunchdDaemonInDomain(stdout io.Writer, servicePath, domain string, force bool) error {
	if _, err := serveInstallStat(servicePath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service file %s does not exist", servicePath)
	} else if err != nil {
		return fmt.Errorf("stat service file %s: %w", servicePath, err)
	}

	target := launchdServiceTarget(domain)
	loaded, state, err := launchdServiceStatus(target)
	if err != nil {
		return err
	}
	if state != "running" && !force {
		return errors.New("daemon is not running (use cleanroom daemon restart --force to start it)")
	}

	if loaded {
		if err := serveInstallRunCommand("launchctl", "bootout", target); err != nil && !isExitError(err) {
			return fmt.Errorf("bootout launchd service %s: %w", launchdServiceName, err)
		}
	}
	if err := serveInstallRunCommand("launchctl", "enable", target); err != nil {
		return fmt.Errorf("enable launchd service %s: %w", launchdServiceName, err)
	}
	if err := serveInstallRunCommand("launchctl", "bootstrap", domain, servicePath); err != nil {
		return fmt.Errorf("bootstrap launchd service %s: %w", launchdServiceName, err)
	}
	if err := serveInstallRunCommand("launchctl", "kickstart", "-k", target); err != nil {
		return fmt.Errorf("restart launchd service %s: %w", launchdServiceName, err)
	}

	_, err = fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon restarted",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "launchd"},
			{Key: "service", Value: launchdServiceName},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func launchdDaemonStatus() (daemonStatusResult, error) {
	return launchdDaemonStatusInDomain(serveInstallLaunchdPath, "system")
}

func launchdUserDaemonStatus() (daemonStatusResult, error) {
	path, err := launchdUserServicePath()
	if err != nil {
		return daemonStatusResult{}, err
	}
	return launchdDaemonStatusInDomain(path, launchdUserDomain())
}

func launchdDaemonStatusInDomain(servicePath, domain string) (daemonStatusResult, error) {
	installed := false
	if _, err := serveInstallStat(servicePath); err == nil {
		installed = true
	}

	_, state, err := launchdServiceStatus(launchdServiceTarget(domain))
	if err != nil {
		return daemonStatusResult{}, err
	}
	active := state == "running"

	listen := ""
	if installed {
		if value, err := launchdConfiguredListenEndpoint(servicePath); err == nil {
			listen = value
		}
	}

	fields := []startupField{
		{Key: "install", Value: daemonInstalledLabel(installed)},
		{Key: "runtime", Value: daemonRuntimeLabel(active)},
		{Key: "domain", Value: domain},
		{Key: "path", Value: servicePath},
	}
	if state != "" {
		fields = append(fields, startupField{Key: "state", Value: state})
	}
	if listen != "" {
		fields = append(fields, startupField{Key: "listen", Value: listen})
	}

	return daemonStatusResult{
		Report: daemonStatusReport{
			Manager:   "launchd",
			Service:   launchdServiceName,
			Installed: installed,
			Active:    active,
			Fields:    fields,
		},
		Payload: daemonStatusPayload{
			Manager:   "launchd",
			Service:   launchdServiceName,
			Installed: installed,
			Active:    active,
			Path:      servicePath,
			Domain:    domain,
			State:     state,
			Listen:    listen,
		},
	}, nil
}

func launchdServiceState(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "state =") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "state ="))
	}
	return ""
}

func launchdServiceLoaded(target string) (bool, error) {
	loaded, _, err := launchdServiceStatus(target)
	return loaded, err
}

func launchdServiceStatus(target string) (bool, string, error) {
	output, err := serveInstallRunCommandOutput("launchctl", "print", target)
	if err == nil {
		return true, launchdServiceState(output), nil
	}
	if isExitError(err) {
		return false, "", nil
	}
	return false, "", fmt.Errorf("check launchd service %s: %w", target, err)
}

func launchdConfiguredListenEndpoint(plistPath string) (string, error) {
	raw, err := os.ReadFile(plistPath)
	if err != nil {
		return "", err
	}

	args, err := launchdProgramArguments(raw)
	if err != nil {
		return "", err
	}

	for i := 0; i < len(args)-1; i++ {
		if strings.TrimSpace(args[i]) == "--listen" {
			return strings.TrimSpace(args[i+1]), nil
		}
	}
	return "", nil
}

func launchdProgramArguments(plistContent []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(plistContent))
	for {
		tok, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}

		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}

		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			return nil, err
		}
		if strings.TrimSpace(key) != "ProgramArguments" {
			continue
		}

		for {
			tok, err = decoder.Token()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil, errors.New("ProgramArguments value missing")
				}
				return nil, err
			}

			start, ok = tok.(xml.StartElement)
			if !ok {
				continue
			}
			if start.Name.Local != "array" {
				return nil, fmt.Errorf("ProgramArguments must be an array, got <%s>", start.Name.Local)
			}
			return plistStringArray(decoder)
		}
	}

	return nil, errors.New("ProgramArguments not found")
}

func plistStringArray(decoder *xml.Decoder) ([]string, error) {
	var values []string
	for {
		tok, err := decoder.Token()
		if err != nil {
			return nil, err
		}

		switch token := tok.(type) {
		case xml.StartElement:
			if token.Name.Local != "string" {
				if err := decoder.Skip(); err != nil {
					return nil, err
				}
				continue
			}

			var value string
			if err := decoder.DecodeElement(&value, &token); err != nil {
				return nil, err
			}
			values = append(values, value)
		case xml.EndElement:
			if token.Name.Local == "array" {
				return values, nil
			}
		}
	}
}

func installLaunchdDaemon(stdout io.Writer, executablePath string, args []string, options daemonInstallOptions) error {
	return installLaunchdDaemonInDomain(stdout, executablePath, args, options, serveInstallLaunchdPath, "system")
}

func installLaunchdUserDaemon(stdout io.Writer, executablePath string, args []string, options daemonInstallOptions) error {
	path, err := launchdUserServicePath()
	if err != nil {
		return err
	}
	return installLaunchdDaemonInDomain(stdout, executablePath, args, options, path, launchdUserDomain())
}

func launchdUserServicePath() (string, error) {
	home, err := serveInstallUserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdServiceName+".plist"), nil
}

func launchdUserDomain() string {
	return fmt.Sprintf("gui/%d", serveInstallUID())
}

func launchdServiceTarget(domain string) string {
	return strings.TrimSpace(domain) + "/" + launchdServiceName
}

func installLaunchdDaemonInDomain(stdout io.Writer, executablePath string, args []string, options daemonInstallOptions, servicePath, domain string) error {
	target := launchdServiceTarget(domain)
	content := renderLaunchdService(executablePath, args)
	loaded, state, err := launchdServiceStatus(target)
	if err != nil {
		return err
	}

	active := state == "running"
	runtimeAction := "unchanged"
	switch {
	case !loaded || !active:
		runtimeAction = "started"
	case options.Restart:
		runtimeAction = "restarted"
	}

	if options.DryRun {
		_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
			Title:      "daemon install dry-run",
			TitleStyle: defaultTerminalPalette().warn,
			Fields: []startupField{
				{Key: "manager", Value: "launchd"},
				{Key: "service", Value: launchdServiceName},
				{Key: "path", Value: servicePath},
				{Key: "runtime", Value: runtimeAction},
			},
		}, shouldUseANSI(stdout)))
		return err
	}

	if err := replaceDaemonFile(servicePath, content, 0o644); err != nil {
		return err
	}

	if loaded && active && !options.Restart {
		if err := serveInstallRunCommand("launchctl", "enable", target); err != nil {
			return fmt.Errorf("enable launchd service %s: %w", launchdServiceName, err)
		}
		_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
			Title:      "daemon installed",
			TitleStyle: defaultTerminalPalette().info,
			Fields: []startupField{
				{Key: "manager", Value: "launchd"},
				{Key: "service", Value: launchdServiceName},
				{Key: "path", Value: servicePath},
			},
		}, shouldUseANSI(stdout)))
		return err
	}

	if loaded {
		if err := serveInstallRunCommand("launchctl", "bootout", target); err != nil && !isExitError(err) {
			return fmt.Errorf("bootout launchd service %s: %w", launchdServiceName, err)
		}
	}
	if err := serveInstallRunCommand("launchctl", "enable", target); err != nil {
		return fmt.Errorf("enable launchd service %s: %w", launchdServiceName, err)
	}
	if err := serveInstallRunCommand("launchctl", "bootstrap", domain, servicePath); err != nil {
		return launchdInstallActionError("bootstrap", target, servicePath, loaded, true, err)
	}
	if err := serveInstallRunCommand("launchctl", "kickstart", "-k", target); err != nil {
		return launchdInstallActionError("start", target, servicePath, loaded, true, err)
	}

	_, err = fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon installed",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "launchd"},
			{Key: "service", Value: launchdServiceName},
			{Key: "path", Value: servicePath},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func validateDaemonFileTarget(path string) error {
	if st, err := serveInstallStat(path); err == nil {
		if st.IsDir() {
			return fmt.Errorf("daemon service path %s is a directory", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return nil
}

func stageDaemonFile(path, content string, mode os.FileMode) (string, func(), error) {
	dir := filepath.Dir(path)
	if err := serveInstallMkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create daemon service directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", nil, fmt.Errorf("create staged daemon service file %s: %w", path, err)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", nil, fmt.Errorf("close staged daemon service file %s: %w", path, err)
	}

	cleanup := func() {
		_ = serveInstallRemoveFile(tmpPath)
	}
	if err := serveInstallWriteFile(tmpPath, []byte(content), mode); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("stage daemon service file %s: %w", path, err)
	}
	return tmpPath, cleanup, nil
}

func replaceDaemonFile(path, content string, mode os.FileMode) error {
	if err := validateDaemonFileTarget(path); err != nil {
		return err
	}

	stagedPath, cleanup, err := stageDaemonFile(path, content, mode)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := serveInstallRename(stagedPath, path); err != nil {
		return fmt.Errorf("replace daemon service file %s: %w", path, err)
	}
	return nil
}

func writeDaemonFile(path, content string, mode os.FileMode) error {
	if err := validateDaemonFileTarget(path); err != nil {
		return err
	}

	if err := serveInstallMkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create daemon service directory: %w", err)
	}
	if err := serveInstallWriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("write daemon service file %s: %w", path, err)
	}
	return nil
}

func launchdInstallActionError(action, target, servicePath string, loaded, updated bool, err error) error {
	return fmt.Errorf("%s launchd service %s (target=%s path=%s loaded=%t updated=%t; if the target is stale, run 'cleanroom daemon uninstall' then 'cleanroom daemon install --restart'): %w",
		action, launchdServiceName, target, servicePath, loaded, updated, err)
}

func renderLaunchdService(executablePath string, args []string) string {
	cmd := append([]string{executablePath}, args...)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	b.WriteString("\t<key>Label</key>\n")
	b.WriteString("\t<string>")
	b.WriteString(escapePlistValue(launchdServiceName))
	b.WriteString("</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n")
	b.WriteString("\t<array>\n")
	for _, arg := range cmd {
		b.WriteString("\t\t<string>")
		b.WriteString(escapePlistValue(arg))
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n")
	b.WriteString("\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n")
	b.WriteString("\t<true/>\n")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String()
}

func escapePlistValue(value string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(value)); err != nil {
		return value
	}
	return b.String()
}

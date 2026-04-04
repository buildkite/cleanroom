package cli

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	launchdStartCheckSleep     = time.Sleep
	launchdBootstrapRetrySleep = time.Sleep
)

type launchdServiceSpec struct {
	Name   string
	Path   string
	Domain string
}

type launchdServiceConfig struct {
	Label            string
	ExecutablePath   string
	Arguments        []string
	RunAtLoad        bool
	KeepAlive        bool
	WorkingDirectory string
	StdoutPath       string
	StderrPath       string
	Environment      map[string]string
}

func launchdServiceSpecForScope(scope daemonScope, service daemonService) (launchdServiceSpec, error) {
	switch scope {
	case daemonScopeSystem:
		return launchdSystemServiceSpec(service), nil
	case daemonScopeUser:
		if service != daemonServiceControl {
			return launchdServiceSpec{}, fmt.Errorf("unsupported launchd user service %q", service)
		}
		return launchdUserServiceSpec()
	default:
		return launchdServiceSpec{}, fmt.Errorf("unsupported launchd scope %q", scope)
	}
}

func launchdSystemServiceSpec(service daemonService) launchdServiceSpec {
	name := launchdSystemServiceName
	path := serveInstallLaunchdPath
	if service == daemonServiceNetworkFilter {
		name = launchdNetworkServiceName
		path = serveInstallLaunchdNetworkPath
	}
	return launchdServiceSpec{
		Name:   name,
		Path:   path,
		Domain: "system",
	}
}

func launchdUserServiceSpec() (launchdServiceSpec, error) {
	path, err := launchdUserServicePath()
	if err != nil {
		return launchdServiceSpec{}, err
	}
	return launchdServiceSpec{
		Name:   launchdUserServiceName,
		Path:   path,
		Domain: launchdUserDomain(),
	}, nil
}

func launchdUserServicePath() (string, error) {
	home, err := serveInstallUserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdUserServiceName+".plist"), nil
}

func launchdUserDomain() string {
	return fmt.Sprintf("gui/%d", serveInstallUID())
}

func launchdServiceTarget(domain, serviceName string) string {
	return strings.TrimSpace(domain) + "/" + strings.TrimSpace(serviceName)
}

func uninstallLaunchdService(stdout io.Writer, spec launchdServiceSpec) error {
	serviceFileExists := false
	if _, err := serveInstallStat(spec.Path); err == nil {
		serviceFileExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat service file %s: %w", spec.Path, err)
	}

	bootoutFoundService := true
	target := launchdServiceTarget(spec.Domain, spec.Name)
	if err := serveInstallRunCommand("launchctl", "bootout", target); err != nil {
		if !isExitError(err) {
			return fmt.Errorf("bootout launchd service %s: %w", spec.Name, err)
		}
		bootoutFoundService = false
	}

	if serviceFileExists {
		if err := serveInstallRemoveFile(spec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove service file %s: %w", spec.Path, err)
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
			{Key: "service", Value: spec.Name},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func startLaunchdService(stdout io.Writer, spec launchdServiceSpec) error {
	if _, err := serveInstallStat(spec.Path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service file %s does not exist", spec.Path)
	} else if err != nil {
		return fmt.Errorf("stat service file %s: %w", spec.Path, err)
	}

	target := launchdServiceTarget(spec.Domain, spec.Name)
	loaded, err := launchdServiceLoaded(target)
	if err != nil {
		return err
	}
	if !loaded {
		if err := serveInstallRunCommand("launchctl", "bootstrap", spec.Domain, spec.Path); err != nil {
			return fmt.Errorf("bootstrap launchd service %s: %w", spec.Name, err)
		}
	}
	if err := serveInstallRunCommand("launchctl", "enable", target); err != nil {
		return fmt.Errorf("enable launchd service %s: %w", spec.Name, err)
	}
	if err := serveInstallRunCommand("launchctl", "kickstart", "-k", target); err != nil {
		return fmt.Errorf("start launchd service %s: %w", spec.Name, err)
	}
	if err := waitForLaunchdServiceRunning(target, spec.Name); err != nil {
		return err
	}

	_, err = fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon started",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "launchd"},
			{Key: "service", Value: spec.Name},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func waitForLaunchdServiceRunning(target, serviceName string) error {
	lastState := ""
	sawExit := false

	for attempt := 0; attempt < 10; attempt++ {
		output, err := serveInstallRunCommandOutput("launchctl", "print", target)
		if err == nil {
			lastState = launchdServiceState(output)
			if lastState == "running" {
				return nil
			}
		} else if isExitError(err) {
			sawExit = true
		} else {
			return fmt.Errorf("check launchd service %s state after start: %w", serviceName, err)
		}

		if attempt < 9 {
			launchdStartCheckSleep(100 * time.Millisecond)
		}
	}

	switch {
	case lastState != "":
		return fmt.Errorf("launchd service %s did not reach running state after start (state: %s)", serviceName, lastState)
	case sawExit:
		return fmt.Errorf("launchd service %s exited before reaching running state", serviceName)
	default:
		return fmt.Errorf("launchd service %s did not reach running state after start", serviceName)
	}
}

func stopLaunchdService(stdout io.Writer, spec launchdServiceSpec) error {
	serviceFileExists := false
	if _, err := serveInstallStat(spec.Path); err == nil {
		serviceFileExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat service file %s: %w", spec.Path, err)
	}

	target := launchdServiceTarget(spec.Domain, spec.Name)
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
				{Key: "service", Value: spec.Name},
			},
		}, shouldUseANSI(stdout)))
		return err
	}
	if !loaded {
		_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
			Title:      "daemon already stopped",
			TitleStyle: defaultTerminalPalette().warn,
			Fields: []startupField{
				{Key: "manager", Value: "launchd"},
				{Key: "service", Value: spec.Name},
			},
		}, shouldUseANSI(stdout)))
		return err
	}

	if err := serveInstallRunCommand("launchctl", "bootout", target); err != nil {
		return fmt.Errorf("stop launchd service %s: %w", spec.Name, err)
	}

	_, err = fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon stopped",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "launchd"},
			{Key: "service", Value: spec.Name},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func launchdDaemonStatusResult(spec launchdServiceSpec) (daemonStatusResult, error) {
	installed := false
	if _, err := serveInstallStat(spec.Path); err == nil {
		installed = true
	}

	active := false
	state := ""
	if output, err := serveInstallRunCommandOutput("launchctl", "print", launchdServiceTarget(spec.Domain, spec.Name)); err == nil {
		state = launchdServiceState(output)
		if state == "running" {
			active = true
		}
	} else if !isExitError(err) {
		return daemonStatusResult{}, fmt.Errorf("check launchd service state: %w", err)
	}

	listen := ""
	if installed {
		if value, err := launchdConfiguredListenEndpoint(spec.Path); err == nil {
			listen = value
		}
	}

	fields := []startupField{
		{Key: "install", Value: daemonInstalledLabel(installed)},
		{Key: "runtime", Value: daemonRuntimeLabel(active)},
		{Key: "domain", Value: spec.Domain},
		{Key: "path", Value: spec.Path},
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
			Service:   spec.Name,
			Installed: installed,
			Active:    active,
			Fields:    fields,
		},
		Payload: daemonStatusPayload{
			Manager:   "launchd",
			Service:   spec.Name,
			Installed: installed,
			Active:    active,
			Path:      spec.Path,
			Domain:    spec.Domain,
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
	if _, err := serveInstallRunCommandOutput("launchctl", "print", target); err == nil {
		return true, nil
	} else if isExitError(err) {
		return false, nil
	} else {
		return false, fmt.Errorf("check launchd service %s: %w", target, err)
	}
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

func installLaunchdService(stdout io.Writer, spec launchdServiceSpec, config launchdServiceConfig, force bool) error {
	content := renderLaunchdService(config)
	target := launchdServiceTarget(spec.Domain, spec.Name)

	if force {
		if err := validateDaemonFileTarget(spec.Path, true); err != nil {
			return err
		}
		stagedPath, cleanup, err := stageDaemonFile(spec.Path, content, 0o644)
		if err != nil {
			return err
		}
		defer cleanup()

		if err := serveInstallRunCommand("launchctl", "bootout", target); err != nil && !isExitError(err) {
			return fmt.Errorf("bootout launchd service %s: %w", spec.Name, err)
		}
		if err := serveInstallRename(stagedPath, spec.Path); err != nil {
			return fmt.Errorf("replace daemon service file %s: %w", spec.Path, err)
		}
	} else {
		if err := writeDaemonFile(spec.Path, content, false, 0o644); err != nil {
			return err
		}
	}

	if err := bootstrapLaunchdService(spec, force); err != nil {
		return fmt.Errorf("bootstrap launchd service %s: %w", spec.Name, err)
	}
	if err := serveInstallRunCommand("launchctl", "enable", target); err != nil {
		return fmt.Errorf("enable launchd service %s: %w", spec.Name, err)
	}

	_, err := fmt.Fprint(stdout, renderSummaryBlock(summaryBlock{
		Title:      "daemon installed",
		TitleStyle: defaultTerminalPalette().info,
		Fields: []startupField{
			{Key: "manager", Value: "launchd"},
			{Key: "service", Value: spec.Name},
			{Key: "path", Value: spec.Path},
		},
	}, shouldUseANSI(stdout)))
	return err
}

func bootstrapLaunchdService(spec launchdServiceSpec, retry bool) error {
	attempts := 1
	if retry {
		attempts = 3
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		lastErr = serveInstallRunCommand("launchctl", "bootstrap", spec.Domain, spec.Path)
		if lastErr == nil {
			return nil
		}
		if !retry || !isExitError(lastErr) || attempt == attempts-1 {
			return lastErr
		}
		launchdBootstrapRetrySleep(100 * time.Millisecond)
	}
	return lastErr
}

func validateDaemonFileTarget(path string, force bool) error {
	if st, err := serveInstallStat(path); err == nil {
		if st.IsDir() {
			return fmt.Errorf("daemon service path %s is a directory", path)
		}
		if !force {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return nil
}

func writeDaemonFile(path, content string, force bool, mode os.FileMode) error {
	if err := validateDaemonFileTarget(path, force); err != nil {
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

func renderLaunchdService(config launchdServiceConfig) string {
	cmd := append([]string{config.ExecutablePath}, config.Arguments...)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	writeLaunchdStringKey(&b, "Label", config.Label)
	writeLaunchdStringArrayKey(&b, "ProgramArguments", cmd)
	writeLaunchdBoolKey(&b, "RunAtLoad", config.RunAtLoad)
	writeLaunchdBoolKey(&b, "KeepAlive", config.KeepAlive)
	writeLaunchdStringKey(&b, "WorkingDirectory", config.WorkingDirectory)
	writeLaunchdStringKey(&b, "StandardOutPath", config.StdoutPath)
	writeLaunchdStringKey(&b, "StandardErrorPath", config.StderrPath)
	writeLaunchdStringMapKey(&b, "EnvironmentVariables", config.Environment)
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String()
}

func writeLaunchdStringKey(b *strings.Builder, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString("\t<key>")
	b.WriteString(escapePlistValue(key))
	b.WriteString("</key>\n")
	b.WriteString("\t<string>")
	b.WriteString(escapePlistValue(value))
	b.WriteString("</string>\n")
}

func writeLaunchdBoolKey(b *strings.Builder, key string, value bool) {
	b.WriteString("\t<key>")
	b.WriteString(escapePlistValue(key))
	b.WriteString("</key>\n")
	if value {
		b.WriteString("\t<true/>\n")
		return
	}
	b.WriteString("\t<false/>\n")
}

func writeLaunchdStringArrayKey(b *strings.Builder, key string, values []string) {
	b.WriteString("\t<key>")
	b.WriteString(escapePlistValue(key))
	b.WriteString("</key>\n")
	b.WriteString("\t<array>\n")
	for _, value := range values {
		b.WriteString("\t\t<string>")
		b.WriteString(escapePlistValue(value))
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n")
}

func writeLaunchdStringMapKey(b *strings.Builder, key string, values map[string]string) {
	if len(values) == 0 {
		return
	}
	keys := make([]string, 0, len(values))
	for mapKey := range values {
		keys = append(keys, mapKey)
	}
	sort.Strings(keys)

	b.WriteString("\t<key>")
	b.WriteString(escapePlistValue(key))
	b.WriteString("</key>\n")
	b.WriteString("\t<dict>\n")
	for _, mapKey := range keys {
		b.WriteString("\t\t<key>")
		b.WriteString(escapePlistValue(mapKey))
		b.WriteString("</key>\n")
		b.WriteString("\t\t<string>")
		b.WriteString(escapePlistValue(values[mapKey]))
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</dict>\n")
}

func escapePlistValue(value string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(value)); err != nil {
		return value
	}
	return b.String()
}

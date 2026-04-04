package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestBuildMacOSAppStampsBundleVersions(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("build-macos-app.sh")
	if err != nil {
		t.Fatalf("read build-macos-app.sh: %v", err)
	}

	script := string(content)
	if !strings.Contains(script, "CLEANROOM_MACOS_BUNDLE_VERSION") {
		t.Fatalf("expected build-macos-app.sh to support overriding the bundle version")
	}
	if !strings.Contains(script, "date -u +%Y%m%d%H%M%S") {
		t.Fatalf("expected build-macos-app.sh to derive a monotonic default bundle version")
	}
	if !strings.Contains(script, "CFBundleVersion") || !strings.Contains(script, "CFBundleShortVersionString") {
		t.Fatalf("expected build-macos-app.sh to stamp app and system-extension bundle versions")
	}
}

func TestBuildMacOSAppSupportsReleasePackagingOverrides(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("build-macos-app.sh")
	if err != nil {
		t.Fatalf("read build-macos-app.sh: %v", err)
	}

	script := string(content)
	for _, needle := range []string{
		"CLEANROOM_MACOS_APP_OUTPUT_PATH",
		"CLEANROOM_MACOS_CLEANROOM_BINARY",
		"CLEANROOM_MACOS_DARWIN_VZ_HELPER_APP",
		"CLEANROOM_MACOS_GUEST_AGENT_BINARY",
		"CLEANROOM_MACOS_SWIFT_TARGET",
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("expected build-macos-app.sh to contain %q", needle)
		}
	}
}

func TestInstallMacOSAppDoesNotUseSystemExtensionsctlUninstall(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("install-macos-app.sh")
	if err != nil {
		t.Fatalf("read install-macos-app.sh: %v", err)
	}

	if strings.Contains(string(content), "systemextensionsctl uninstall") {
		t.Fatalf("expected install-macos-app.sh to rely on normal versioned system-extension replacement")
	}
}

func TestUninstallMacOSAppDoesNotUseSystemExtensionsctlUninstall(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("uninstall-macos-app.sh")
	if err != nil {
		t.Fatalf("read uninstall-macos-app.sh: %v", err)
	}

	if strings.Contains(string(content), "systemextensionsctl uninstall") {
		t.Fatalf("expected uninstall-macos-app.sh to avoid systemextensionsctl uninstall when SIP is enabled")
	}
}

func TestUninstallMacOSAppSystemTaskStillUsesSudo(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../.mise.toml")
	if err != nil {
		t.Fatalf("read .mise.toml: %v", err)
	}

	if !strings.Contains(string(content), "CLEANROOM_APP_INSTALL_DIR=\\\"/Applications\\\" CLEANROOM_APP_USE_SUDO=\\\"1\\\" scripts/uninstall-macos-app.sh") {
		t.Fatalf("expected uninstall:macos-app-system to pass CLEANROOM_APP_USE_SUDO=1")
	}
}

func TestFilterProviderInfoPlistMatchesObjCRuntimeClassName(t *testing.T) {
	t.Helper()

	swiftContent, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}
	plistContent, err := os.ReadFile("../macos/CleanroomFilterDataProvider/Info.plist")
	if err != nil {
		t.Fatalf("read provider Info.plist: %v", err)
	}

	if !strings.Contains(string(swiftContent), "@objc(CleanroomFilterDataProvider)") {
		t.Fatalf("expected provider.swift to declare the Objective-C runtime name explicitly")
	}
	if strings.Contains(string(plistContent), "<string>CleanroomFilterDataProvider.CleanroomFilterDataProvider</string>") {
		t.Fatalf("expected Info.plist not to use the Swift namespaced class name when provider.swift sets an explicit Objective-C runtime name")
	}
	if !strings.Contains(string(plistContent), "<string>CleanroomFilterDataProvider</string>") {
		t.Fatalf("expected Info.plist to register the Objective-C runtime class name for NEProviderClasses")
	}
}

func TestFilterProviderUsesPacketExtensionPoint(t *testing.T) {
	t.Helper()

	plistContent, err := os.ReadFile("../macos/CleanroomFilterDataProvider/Info.plist")
	if err != nil {
		t.Fatalf("read provider Info.plist: %v", err)
	}

	source := string(plistContent)
	if !strings.Contains(source, "<key>com.apple.networkextension.filter-packet</key>") {
		t.Fatalf("expected Info.plist to register a packet-filter provider")
	}
	if strings.Contains(source, "<key>com.apple.networkextension.filter-data</key>") {
		t.Fatalf("expected Info.plist not to register a flow-filter provider")
	}
}

func TestBuildMacOSAppValidatesPacketProviderMetadata(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("build-macos-app.sh")
	if err != nil {
		t.Fatalf("read build-macos-app.sh: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "com.apple.networkextension.filter-packet") {
		t.Fatalf("expected build-macos-app.sh to validate packet-provider metadata")
	}
	if strings.Contains(source, "com.apple.networkextension.filter-data") {
		t.Fatalf("expected build-macos-app.sh not to validate the old flow-provider metadata path")
	}
}

func TestFilterProviderUsesNetworkFilterDaemon(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "NetworkFilterDaemonClient()") {
		t.Fatalf("expected provider.swift to create a network-filter daemon client")
	}
	if !strings.Contains(source, "daemonClient.getPolicy()") {
		t.Fatalf("expected provider.swift to load policy snapshots from the network-filter daemon")
	}
	if !strings.Contains(source, "daemonClient.patchStatus") {
		t.Fatalf("expected provider.swift to publish provider status through the network-filter daemon")
	}
}

func TestFilterProviderUsesPacketProviderAPI(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "NEFilterPacketProvider") {
		t.Fatalf("expected provider.swift to subclass NEFilterPacketProvider")
	}
	if !strings.Contains(source, "handler =") {
		t.Fatalf("expected provider.swift to install a packet handler")
	}
	if strings.Contains(source, "handleNewFlow") {
		t.Fatalf("expected provider.swift not to keep flow-based handleNewFlow logic")
	}
}

func TestFilterProviderEvaluatesGuestScopedPacketRules(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "guestRules") {
		t.Fatalf("expected provider.swift to evaluate guest-scoped packet rules")
	}
	if !strings.Contains(source, ".drop") {
		t.Fatalf("expected provider.swift to emit a drop verdict for denied guest egress")
	}
}

func TestFilterProviderPublishesTraceSamplesForGuestDebugging(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}

	source := string(content)
	for _, needle := range []string{
		"provider_trace_packets",
		"provider_trace_policy_guest_ips",
		"matchRole",
	} {
		if !strings.Contains(source, needle) {
			t.Fatalf("expected provider.swift to contain %q for packet trace diagnostics", needle)
		}
	}
}

func TestFilterProviderAvoidsBlockingWorkInPacketHandler(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "interface.type == .loopback") {
		t.Fatalf("expected provider.swift to fast-path loopback traffic")
	}
	if !strings.Contains(source, "flushPacketObservationIfNeeded") {
		t.Fatalf("expected provider.swift to flush packet telemetry outside the packet handler")
	}

	observePacketIndex := strings.Index(source, "private func observePacket")
	if observePacketIndex == -1 {
		t.Fatalf("expected provider.swift to define observePacket")
	}
	flushIndex := strings.Index(source, "private func flushPacketObservationIfNeeded")
	if flushIndex == -1 || flushIndex <= observePacketIndex {
		t.Fatalf("expected provider.swift to define flushPacketObservationIfNeeded after observePacket")
	}
	observePacketBody := source[observePacketIndex:flushIndex]
	if strings.Contains(observePacketBody, "writeProviderStatus") || strings.Contains(observePacketBody, "patchStatus") {
		t.Fatalf("expected observePacket not to call daemon status writes directly")
	}
}

func TestFilterProviderIncludesFirstLightDiagnostics(t *testing.T) {
	t.Helper()

	mainContent, err := os.ReadFile("../macos/CleanroomFilterDataProvider/main.swift")
	if err != nil {
		t.Fatalf("read provider main.swift: %v", err)
	}
	providerContent, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}

	if !strings.Contains(string(mainContent), "system extension main entry") {
		t.Fatalf("expected provider main.swift to emit a first-light bootstrap log")
	}
	if !strings.Contains(string(providerContent), "provider init") || !strings.Contains(string(providerContent), "startFilter invoked") {
		t.Fatalf("expected provider.swift to emit init/startFilter diagnostics")
	}
}

func TestFilterProviderClearsProviderLastErrorWhenHealthy(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "payload[\"provider_last_error\"] = loadResult.errorDetail ?? NSNull()") {
		t.Fatalf("expected provider.swift to clear provider_last_error via NSNull when startFilter succeeds")
	}
	if !strings.Contains(source, "payload[\"provider_last_error\"] = NSNull()") {
		t.Fatalf("expected provider.swift to clear provider_last_error via NSNull when recording successful packet observations")
	}
}

func TestMacOSAppDoesNotAttemptPrivilegedDaemonInstall(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/main.swift")
	if err != nil {
		t.Fatalf("read main.swift: %v", err)
	}

	source := string(content)
	if strings.Contains(source, "with administrator privileges") {
		t.Fatalf("expected the macOS app not to request privileged daemon installation directly")
	}
	if strings.Contains(source, "installNetworkFilterDaemon()") {
		t.Fatalf("expected the macOS app to delegate network daemon installation to the CLI")
	}
}

func TestMacOSAppClearsLastErrorWhenStatusRecovers(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/main.swift")
	if err != nil {
		t.Fatalf("read main.swift: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "value[\"last_error\"] = NSNull()") {
		t.Fatalf("expected main.swift to clear last_error via NSNull when network filter status is healthy")
	}
}

func TestMacOSAppPersistsExtensionAvailabilityOnEnablePath(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/main.swift")
	if err != nil {
		t.Fatalf("read main.swift: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "networkFilterAvailable = isNetworkFilterExtensionInstalled()") {
		t.Fatalf("expected main.swift to derive extension availability when updating filter status")
	}
}

func TestMacOSAppValidatesProviderStartupUsingStartedAtBeforeUpdatedAt(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/main.swift")
	if err != nil {
		t.Fatalf("read main.swift: %v", err)
	}

	source := string(content)
	startedIndex := strings.Index(source, "if let providerStartedAt = parseNetworkFilterTimestamp(status.providerStartedAt),")
	if startedIndex == -1 {
		t.Fatalf("expected main.swift to validate provider startup using providerStartedAt")
	}
	updatedIndex := strings.Index(source, "if let providerUpdatedAt = parseNetworkFilterTimestamp(status.providerUpdatedAt),")
	if updatedIndex == -1 {
		t.Fatalf("expected main.swift to consider providerUpdatedAt when validating startup")
	}
	if startedIndex > updatedIndex {
		t.Fatalf("expected main.swift to check providerStartedAt before providerUpdatedAt when validating startup")
	}
}

func TestMacOSAppEnablesPacketFiltering(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile("../macos/main.swift")
	if err != nil {
		t.Fatalf("read main.swift: %v", err)
	}

	source := string(content)
	if !strings.Contains(source, "configuration.filterPackets = true") {
		t.Fatalf("expected main.swift to enable packet filtering")
	}
	if strings.Contains(source, "configuration.filterSockets = true") {
		t.Fatalf("expected main.swift not to enable socket flow filtering in packet-mode prototype")
	}
}

func TestMacOSAppDoesNotIncludeStatusItemUI(t *testing.T) {
	t.Helper()

	mainContent, err := os.ReadFile("../macos/main.swift")
	if err != nil {
		t.Fatalf("read main.swift: %v", err)
	}
	projectContent, err := os.ReadFile("../macos/project.yml")
	if err != nil {
		t.Fatalf("read project.yml: %v", err)
	}
	scriptContent, err := os.ReadFile("build-macos-app.sh")
	if err != nil {
		t.Fatalf("read build-macos-app.sh: %v", err)
	}

	source := string(mainContent)
	if strings.Contains(source, "NSStatusBar.system.statusItem") {
		t.Fatalf("expected main.swift not to create a persistent status-item UI")
	}
	if strings.Contains(source, "NSMenuItem(title: \"Enable Network Filter\"") {
		t.Fatalf("expected main.swift not to define menu-bar actions for network filter control")
	}
	if strings.Contains(source, "networkFilterStatusRefreshIntervalSeconds") {
		t.Fatalf("expected main.swift not to run a menu-bar refresh timer")
	}
	if strings.Contains(string(projectContent), "menubar-icon.png") || strings.Contains(string(projectContent), "menubar-icon@2x.png") {
		t.Fatalf("expected project.yml not to bundle menu-bar icon resources")
	}
	if strings.Contains(string(scriptContent), "MENUBAR_ICON_SRC") || strings.Contains(string(scriptContent), "MENUBAR_ICON_2X_SRC") {
		t.Fatalf("expected build-macos-app.sh not to install menu-bar icon resources")
	}
}

func TestMacOSAppDoesNotRequireAppGroups(t *testing.T) {
	t.Helper()

	appEntitlements, err := os.ReadFile("../macos/entitlements.plist")
	if err != nil {
		t.Fatalf("read macos/entitlements.plist: %v", err)
	}
	filterEntitlements, err := os.ReadFile("../macos/CleanroomFilterDataProvider/entitlements.plist")
	if err != nil {
		t.Fatalf("read filter entitlements.plist: %v", err)
	}
	filterDeveloperIDEntitlements, err := os.ReadFile("../macos/CleanroomFilterDataProvider/entitlements-developer-id.plist")
	if err != nil {
		t.Fatalf("read filter entitlements-developer-id.plist: %v", err)
	}
	projectContent, err := os.ReadFile("../macos/project.yml")
	if err != nil {
		t.Fatalf("read macos/project.yml: %v", err)
	}
	scriptContent, err := os.ReadFile("build-macos-app.sh")
	if err != nil {
		t.Fatalf("read build-macos-app.sh: %v", err)
	}

	for _, content := range []string{string(appEntitlements), string(filterEntitlements), string(filterDeveloperIDEntitlements)} {
		if strings.Contains(content, "com.apple.security.application-groups") {
			t.Fatalf("expected macOS app entitlements not to require App Groups")
		}
	}
	if strings.Contains(string(projectContent), "REGISTER_APP_GROUPS: YES") {
		t.Fatalf("expected project.yml not to register App Groups")
	}
	if strings.Contains(string(scriptContent), "APP_GROUP_IDENTIFIER") || strings.Contains(string(scriptContent), "profile_supports_app_group") {
		t.Fatalf("expected build-macos-app.sh not to validate provisioning profiles against an App Group")
	}
}

func TestMacOSAppUsesNetworkBundleIdentifiers(t *testing.T) {
	t.Helper()

	appInfo, err := os.ReadFile("../macos/Info.plist")
	if err != nil {
		t.Fatalf("read macos/Info.plist: %v", err)
	}
	filterInfo, err := os.ReadFile("../macos/CleanroomFilterDataProvider/Info.plist")
	if err != nil {
		t.Fatalf("read filter Info.plist: %v", err)
	}
	mainContent, err := os.ReadFile("../macos/main.swift")
	if err != nil {
		t.Fatalf("read macos/main.swift: %v", err)
	}
	providerMain, err := os.ReadFile("../macos/CleanroomFilterDataProvider/main.swift")
	if err != nil {
		t.Fatalf("read provider main.swift: %v", err)
	}
	providerSwift, err := os.ReadFile("../macos/CleanroomFilterDataProvider/provider.swift")
	if err != nil {
		t.Fatalf("read provider.swift: %v", err)
	}
	projectContent, err := os.ReadFile("../macos/project.yml")
	if err != nil {
		t.Fatalf("read macos/project.yml: %v", err)
	}
	installScript, err := os.ReadFile("install-macos-app.sh")
	if err != nil {
		t.Fatalf("read install-macos-app.sh: %v", err)
	}
	uninstallScript, err := os.ReadFile("uninstall-macos-app.sh")
	if err != nil {
		t.Fatalf("read uninstall-macos-app.sh: %v", err)
	}

	for _, content := range []string{
		string(appInfo),
		string(filterInfo),
		string(mainContent),
		string(providerMain),
		string(providerSwift),
		string(projectContent),
		string(installScript),
		string(uninstallScript),
	} {
		if strings.Contains(content, "com.buildkite.cleanroom.menubar") {
			t.Fatalf("expected macOS network utility sources not to reference the old menubar bundle identifiers")
		}
	}
	if !strings.Contains(string(appInfo), "com.buildkite.cleanroom.network") {
		t.Fatalf("expected app Info.plist to use com.buildkite.cleanroom.network")
	}
	if !strings.Contains(string(filterInfo), "com.buildkite.cleanroom.network.filter") {
		t.Fatalf("expected filter Info.plist to use com.buildkite.cleanroom.network.filter")
	}
}

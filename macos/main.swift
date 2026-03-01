import AppKit
import Darwin
import Foundation
import NetworkExtension

private enum AppConstants {
    static let statusIconFallback = "👩‍🔬"
    static let statusIconResource = "menubar-icon"
    static let statusIconExtension = "png"
    static let statusIcon2xResource = "menubar-icon@2x"
    static let menuTitle = "Cleanroom"
    static let networkFilterDescription = "Cleanroom Network Filter"
    static let networkFilterOrganization = "Buildkite Cleanroom"
    static let networkFilterProviderBundleName = "CleanroomFilterDataProvider.appex"
    static let networkFilterProviderExecutableName = "CleanroomFilterDataProvider"
    static let networkFilterPolicyRelativePath = "Library/Application Support/Cleanroom/network-filter-policy.json"
    static let networkFilterPolicyPathEnv = "CLEANROOM_NETWORK_FILTER_POLICY_PATH"
    static let networkFilterTargetProcessEnv = "CLEANROOM_NETWORK_FILTER_TARGET_PROCESS"
    static let networkFilterPolicyPathVendorKey = "policy_path"
    static let networkFilterTargetProcessVendorKey = "target_process_path"
    static let appLogRelativePath = "Library/Logs/cleanroom-menubar.log"
    static let serviceLogRelativePath = "Library/Logs/cleanroom-user-server.log"
    static let launchdSystemPlistPath = "/Library/LaunchDaemons/com.buildkite.cleanroom.plist"
    static let userLaunchAgentLabel = "com.buildkite.cleanroom.user"
    static let userLaunchAgentRelativePath = "Library/LaunchAgents/com.buildkite.cleanroom.user.plist"
    static let runOnStartupPreferenceKey = "runServerAtLogin"
    static let cliBinDir = "/usr/local/bin"
    static let cliSymlinkPath = "/usr/local/bin/cleanroom"
    static let cleanroomBinaryOverrideEnv = "CLEANROOM_BINARY"
}

private struct CommandResult {
    let terminationStatus: Int32
    let stdout: String
    let stderr: String
}

private enum AppError: LocalizedError {
    case commandFailed(command: String, details: String)
    case invalidLaunchAgentPlist

    var errorDescription: String? {
        switch self {
        case .commandFailed(let command, let details):
            return "\(command) failed: \(details)"
        case .invalidLaunchAgentPlist:
            return "failed to generate launch agent plist"
        }
    }
}

final class CleanroomMenuBarApp: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private var statusItem: NSStatusItem!
    private var menu: NSMenu!

    private var serverStatusItem: NSMenuItem!
    private var cliStatusItem: NSMenuItem!
    private var daemonStatusItem: NSMenuItem!
    private var networkFilterStatusItem: NSMenuItem!

    private var enableItem: NSMenuItem!
    private var runOnStartupItem: NSMenuItem!
    private var enableNetworkFilterItem: NSMenuItem!
    private var disableNetworkFilterItem: NSMenuItem!
    private var advancedRestartItem: NSMenuItem!
    private var advancedStopItem: NSMenuItem!
    private var advancedInstallDaemonItem: NSMenuItem!
    private var logsItem: NSMenuItem!

    private var appLogHandle: FileHandle?
    private var networkFilterAvailable = false
    private var networkFilterLoaded = false
    private var networkFilterEnabled = false
    private var networkFilterLastError: String?

    private lazy var appLogURL: URL = {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(AppConstants.appLogRelativePath, isDirectory: false)
    }()

    private lazy var serviceLogURL: URL = {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(AppConstants.serviceLogRelativePath, isDirectory: false)
    }()

    private lazy var networkFilterPolicyURL: URL = {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(AppConstants.networkFilterPolicyRelativePath, isDirectory: false)
    }()

    private lazy var launchAgentURL: URL = {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(AppConstants.userLaunchAgentRelativePath, isDirectory: false)
    }()

    private lazy var resolvedBinaryURL: URL? = resolveCleanroomBinary()

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        configureStatusButton(statusItem.button)

        menu = NSMenu()
        menu.delegate = self

        serverStatusItem = NSMenuItem(title: "Server: stopped", action: nil, keyEquivalent: "")
        serverStatusItem.isEnabled = false
        menu.addItem(serverStatusItem)

        cliStatusItem = NSMenuItem(title: "CLI: not installed", action: nil, keyEquivalent: "")
        cliStatusItem.isEnabled = false
        menu.addItem(cliStatusItem)

        daemonStatusItem = NSMenuItem(title: "System daemon: unknown", action: nil, keyEquivalent: "")
        daemonStatusItem.isEnabled = false
        menu.addItem(daemonStatusItem)

        networkFilterStatusItem = NSMenuItem(title: "Network filter: checking...", action: nil, keyEquivalent: "")
        networkFilterStatusItem.isEnabled = false
        menu.addItem(networkFilterStatusItem)

        menu.addItem(NSMenuItem.separator())

        enableItem = NSMenuItem(title: "Enable Cleanroom", action: #selector(enableCleanroom), keyEquivalent: "e")
        enableItem.target = self
        menu.addItem(enableItem)

        logsItem = NSMenuItem(title: "Open Logs", action: #selector(openLogs), keyEquivalent: "l")
        logsItem.target = self
        menu.addItem(logsItem)

        let advancedMenu = NSMenu(title: "Advanced")
        runOnStartupItem = NSMenuItem(title: "Run Server At Login", action: #selector(toggleRunOnStartup), keyEquivalent: "")
        runOnStartupItem.target = self
        advancedMenu.addItem(runOnStartupItem)

        advancedMenu.addItem(NSMenuItem.separator())

        enableNetworkFilterItem = NSMenuItem(title: "Enable Network Filter", action: #selector(enableNetworkFilter), keyEquivalent: "")
        enableNetworkFilterItem.target = self
        advancedMenu.addItem(enableNetworkFilterItem)

        disableNetworkFilterItem = NSMenuItem(title: "Disable Network Filter", action: #selector(disableNetworkFilter), keyEquivalent: "")
        disableNetworkFilterItem.target = self
        advancedMenu.addItem(disableNetworkFilterItem)

        advancedMenu.addItem(NSMenuItem.separator())

        advancedRestartItem = NSMenuItem(title: "Restart User Server", action: #selector(restartUserServer), keyEquivalent: "")
        advancedRestartItem.target = self
        advancedMenu.addItem(advancedRestartItem)

        advancedStopItem = NSMenuItem(title: "Stop User Server", action: #selector(stopServer), keyEquivalent: "")
        advancedStopItem.target = self
        advancedMenu.addItem(advancedStopItem)

        advancedMenu.addItem(NSMenuItem.separator())

        advancedInstallDaemonItem = NSMenuItem(title: "Install System Daemon (Admin)", action: #selector(installDaemon), keyEquivalent: "")
        advancedInstallDaemonItem.target = self
        advancedMenu.addItem(advancedInstallDaemonItem)

        let advancedItem = NSMenuItem(title: "Advanced", action: nil, keyEquivalent: "")
        menu.setSubmenu(advancedMenu, for: advancedItem)
        menu.addItem(advancedItem)

        menu.addItem(NSMenuItem.separator())

        let quitItem = NSMenuItem(title: "Quit", action: #selector(quitApp), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)

        statusItem.menu = menu
        refreshNetworkFilterStatus()
        refreshUI()
    }

    func applicationWillTerminate(_ notification: Notification) {
        closeAppLogHandle()
    }

    func menuWillOpen(_ menu: NSMenu) {
        refreshNetworkFilterStatus()
        refreshUI()
    }

    @objc private func enableCleanroom(_ sender: Any?) {
        guard let binaryURL = resolvedBinaryURL else {
            presentError("cleanroom binary not found. Set \(AppConstants.cleanroomBinaryOverrideEnv) or install cleanroom.")
            refreshUI()
            return
        }

        do {
            try startOrRestartUserService(binaryURL: binaryURL)
            if !areRequiredCLISymlinksInstalled(cleanroomBinaryURL: binaryURL) {
                try installRequiredCLISymlinksWithAdmin(cleanroomBinaryURL: binaryURL)
                appendLog("installed CLI symlink at \(AppConstants.cliSymlinkPath)")
            }
            appendLog("enabled cleanroom")
            showInfo("Cleanroom is enabled.\nUser service is running and CLI is linked at \(AppConstants.cliSymlinkPath).")
            refreshUI()
        } catch {
            presentError("failed to enable cleanroom: \(error.localizedDescription)")
            refreshUI()
        }
    }

    @objc private func restartUserServer(_ sender: Any?) {
        guard let binaryURL = resolvedBinaryURL else {
            presentError("cleanroom binary not found. Set \(AppConstants.cleanroomBinaryOverrideEnv) or install cleanroom.")
            refreshUI()
            return
        }

        do {
            try startOrRestartUserService(binaryURL: binaryURL)
            appendLog("restarted user server via advanced menu")
            refreshUI()
        } catch {
            presentError("failed to restart user server: \(error.localizedDescription)")
            refreshUI()
        }
    }

    @objc private func toggleRunOnStartup(_ sender: Any?) {
        guard let binaryURL = resolvedBinaryURL else {
            presentError("cleanroom binary not found. Set \(AppConstants.cleanroomBinaryOverrideEnv) or install cleanroom.")
            refreshUI()
            return
        }

        let enabled = !isRunOnStartupEnabled()
        setRunOnStartupEnabled(enabled)

        do {
            if FileManager.default.fileExists(atPath: launchAgentURL.path) {
                try ensureUserLaunchAgentPlist(binaryURL: binaryURL)
            }
            appendLog("set run-on-startup to \(enabled)")
            if !enabled {
                showInfo("Run Server At Login disabled. This takes effect on next login.")
            }
            refreshUI()
        } catch {
            presentError("failed to update startup setting: \(error.localizedDescription)")
            refreshUI()
        }
    }

    @objc private func enableNetworkFilter(_ sender: Any?) {
        saveNetworkFilter(enabled: true)
    }

    @objc private func disableNetworkFilter(_ sender: Any?) {
        saveNetworkFilter(enabled: false)
    }

    @objc private func stopServer(_ sender: Any?) {
        do {
            let serviceTarget = userServiceTarget()
            let bootoutResult = try runCommandAllowFailure("/bin/launchctl", ["bootout", serviceTarget])
            if bootoutResult.terminationStatus != 0 && queryUserServiceLoaded() {
                throw AppError.commandFailed(
                    command: commandDescription("/bin/launchctl", ["bootout", serviceTarget]),
                    details: commandDetails(bootoutResult)
                )
            }

            appendLog("stopped user server via launchctl bootout")
            refreshUI()
        } catch {
            presentError("failed to stop user server: \(error.localizedDescription)")
            refreshUI()
        }
    }

    private func startOrRestartUserService(binaryURL: URL) throws {
        try ensureUserLaunchAgentPlist(binaryURL: binaryURL)
        let domainTarget = userLaunchdDomain()
        let serviceTarget = userServiceTarget()

        if queryUserServiceLoaded() {
            _ = try runCommand("/bin/launchctl", ["kickstart", "-k", serviceTarget])
            return
        }

        let bootstrapResult = try runCommandAllowFailure("/bin/launchctl", ["bootstrap", domainTarget, launchAgentURL.path])
        if bootstrapResult.terminationStatus != 0 && !queryUserServiceLoaded() {
            throw AppError.commandFailed(
                command: commandDescription("/bin/launchctl", ["bootstrap", domainTarget, launchAgentURL.path]),
                details: commandDetails(bootstrapResult)
            )
        }
        _ = try runCommandAllowFailure("/bin/launchctl", ["enable", serviceTarget])
        _ = try runCommand("/bin/launchctl", ["kickstart", "-k", serviceTarget])
    }

    @objc private func installDaemon(_ sender: Any?) {
        guard let binaryURL = resolvedBinaryURL else {
            presentError("cleanroom binary not found. Cannot install daemon.")
            return
        }

        let shellCommand = "\(shellQuote(binaryURL.path)) serve install --force"
        let script = "do shell script \"\(escapeForAppleScript(shellCommand))\" with administrator privileges"

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
        proc.arguments = ["-e", script]
        let err = Pipe()
        proc.standardOutput = Pipe()
        proc.standardError = err

        do {
            try proc.run()
            proc.waitUntilExit()
        } catch {
            presentError("failed to run installer: \(error.localizedDescription)")
            return
        }

        let stderr = readAllString(from: err.fileHandleForReading)
        if proc.terminationStatus != 0 {
            let details = stderr.isEmpty ? "command failed" : stderr
            presentError("daemon install failed: \(details)")
            return
        }

        appendLog("installed system daemon with serve install --force")
        showInfo("System daemon installed.")
        refreshUI()
    }

    @objc private func openLogs(_ sender: Any?) {
        if FileManager.default.fileExists(atPath: serviceLogURL.path) {
            NSWorkspace.shared.open(serviceLogURL)
            return
        }
        _ = try? openAppLogHandle()
        NSWorkspace.shared.open(appLogURL)
    }

    @objc private func quitApp(_ sender: Any?) {
        NSApp.terminate(nil)
    }

    private func refreshUI() {
        let running = queryUserServiceLoaded()
        let installed = FileManager.default.fileExists(atPath: launchAgentURL.path)
        let binaryAvailable = resolvedBinaryURL != nil
        let cliInstalled = areRequiredCLISymlinksInstalled(cleanroomBinaryURL: resolvedBinaryURL)
        let runOnStartupEnabled = isRunOnStartupEnabled()

        if running {
            serverStatusItem.title = "Server: running (LaunchAgent)"
        } else if installed {
            serverStatusItem.title = "Server: stopped (LaunchAgent installed)"
        } else {
            serverStatusItem.title = "Server: stopped (LaunchAgent not installed)"
        }

        if cliInstalled {
            cliStatusItem.title = "CLI: installed (\(AppConstants.cliBinDir))"
        } else {
            cliStatusItem.title = "CLI: not installed"
        }

        if FileManager.default.fileExists(atPath: AppConstants.launchdSystemPlistPath) {
            daemonStatusItem.title = "System daemon: installed"
        } else {
            daemonStatusItem.title = "System daemon: not installed"
        }

        if let error = networkFilterLastError {
            networkFilterStatusItem.title = "Network filter: unavailable (\(statusErrorSummary(error)))"
        } else if !networkFilterLoaded {
            networkFilterStatusItem.title = "Network filter: checking..."
        } else if networkFilterEnabled {
            networkFilterStatusItem.title = "Network filter: enabled"
        } else {
            networkFilterStatusItem.title = "Network filter: disabled"
        }

        if binaryAvailable {
            if running && cliInstalled {
                enableItem.title = "Repair Cleanroom"
            } else {
                enableItem.title = "Enable Cleanroom"
            }
        } else {
            enableItem.title = "Enable Cleanroom"
        }

        enableItem.isEnabled = binaryAvailable
        runOnStartupItem.state = runOnStartupEnabled ? .on : .off
        runOnStartupItem.isEnabled = binaryAvailable
        enableNetworkFilterItem.isEnabled = binaryAvailable && networkFilterAvailable && networkFilterLoaded && !networkFilterEnabled
        disableNetworkFilterItem.isEnabled = binaryAvailable && networkFilterAvailable && networkFilterLoaded && networkFilterEnabled
        advancedRestartItem.isEnabled = binaryAvailable
        advancedStopItem.isEnabled = running
        advancedInstallDaemonItem.isEnabled = binaryAvailable
        logsItem.isEnabled = true
    }

    private func configureStatusButton(_ button: NSStatusBarButton?) {
        guard let button else {
            return
        }
        button.toolTip = AppConstants.menuTitle

        if let icon = loadStatusIcon() {
            button.image = icon
            button.imagePosition = .imageOnly
            button.title = ""
            return
        }

        button.image = nil
        button.title = AppConstants.statusIconFallback
    }

    private func loadStatusIcon() -> NSImage? {
        guard
            let basePath = Bundle.main.path(
                forResource: AppConstants.statusIconResource,
                ofType: AppConstants.statusIconExtension
            ),
            let icon = NSImage(contentsOfFile: basePath)
        else {
            return nil
        }

        if
            let retinaPath = Bundle.main.path(
                forResource: AppConstants.statusIcon2xResource,
                ofType: AppConstants.statusIconExtension
            ),
            let retinaImage = NSImage(contentsOfFile: retinaPath),
            let retinaRep = retinaImage.representations.first
        {
            icon.addRepresentation(retinaRep)
        }

        icon.size = NSSize(width: 16, height: 16)
        icon.isTemplate = true
        return icon
    }

    private func ensureUserLaunchAgentPlist(binaryURL: URL) throws {
        let fm = FileManager.default
        try fm.createDirectory(at: launchAgentURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        try fm.createDirectory(at: serviceLogURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        try fm.createDirectory(at: networkFilterPolicyURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        if !fm.fileExists(atPath: serviceLogURL.path) {
            fm.createFile(atPath: serviceLogURL.path, contents: Data())
        }
        let runOnStartupEnabled = isRunOnStartupEnabled()
        var environment: [String: String] = [
            AppConstants.networkFilterPolicyPathEnv: networkFilterPolicyURL.path,
        ]
        if let helperURL = resolveDarwinVZHelperBinary() {
            environment[AppConstants.networkFilterTargetProcessEnv] = helperURL.path
        }

        let plist: [String: Any] = [
            "Label": AppConstants.userLaunchAgentLabel,
            "ProgramArguments": [binaryURL.path, "serve"],
            "RunAtLoad": runOnStartupEnabled,
            "KeepAlive": runOnStartupEnabled,
            "EnvironmentVariables": environment,
            "WorkingDirectory": fm.homeDirectoryForCurrentUser.path,
            "StandardOutPath": serviceLogURL.path,
            "StandardErrorPath": serviceLogURL.path,
        ]

        guard PropertyListSerialization.propertyList(plist, isValidFor: .xml) else {
            throw AppError.invalidLaunchAgentPlist
        }

        let data = try PropertyListSerialization.data(
            fromPropertyList: plist,
            format: .xml,
            options: 0
        )
        try data.write(to: launchAgentURL, options: .atomic)
    }

    private func userLaunchdDomain() -> String {
        "gui/\(getuid())"
    }

    private func userServiceTarget() -> String {
        "\(userLaunchdDomain())/\(AppConstants.userLaunchAgentLabel)"
    }

    private func queryUserServiceLoaded() -> Bool {
        do {
            let result = try runCommandAllowFailure("/bin/launchctl", ["print", userServiceTarget()])
            return result.terminationStatus == 0
        } catch {
            appendLog("launchctl print failed: \(error.localizedDescription)")
            return false
        }
    }

    private func runCommand(_ executable: String, _ args: [String]) throws -> CommandResult {
        let result = try runCommandAllowFailure(executable, args)
        if result.terminationStatus != 0 {
            throw AppError.commandFailed(
                command: commandDescription(executable, args),
                details: commandDetails(result)
            )
        }
        return result
    }

    private func runCommandAllowFailure(_ executable: String, _ args: [String]) throws -> CommandResult {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: executable)
        proc.arguments = args
        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        proc.standardOutput = stdoutPipe
        proc.standardError = stderrPipe
        try proc.run()
        proc.waitUntilExit()
        return CommandResult(
            terminationStatus: proc.terminationStatus,
            stdout: readAllString(from: stdoutPipe.fileHandleForReading),
            stderr: readAllString(from: stderrPipe.fileHandleForReading)
        )
    }

    private func commandDescription(_ executable: String, _ args: [String]) -> String {
        ([executable] + args).joined(separator: " ")
    }

    private func commandDetails(_ result: CommandResult) -> String {
        let merged = [result.stdout, result.stderr]
            .joined(separator: "\n")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        if merged.isEmpty {
            return "exit status \(result.terminationStatus)"
        }
        return merged
    }

    private func statusErrorSummary(_ value: String) -> String {
        let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty {
            return "unknown"
        }
        let maxChars = 72
        if trimmed.count <= maxChars {
            return trimmed
        }
        let idx = trimmed.index(trimmed.startIndex, offsetBy: maxChars)
        return String(trimmed[..<idx]) + "..."
    }

    private func refreshNetworkFilterStatus() {
        if !isNetworkFilterExtensionInstalled() {
            networkFilterAvailable = false
            networkFilterLoaded = false
            networkFilterEnabled = false
            networkFilterLastError = "filter extension is not bundled in Cleanroom.app"
            refreshUI()
            return
        }

        networkFilterAvailable = true
        loadNetworkFilterManager { [weak self] manager, error in
            guard let self else {
                return
            }
            if let error {
                self.networkFilterLoaded = false
                self.networkFilterEnabled = false
                self.networkFilterLastError = error.localizedDescription
            } else {
                self.networkFilterLoaded = true
                self.networkFilterEnabled = manager?.isEnabled ?? false
                self.networkFilterLastError = nil
            }
            self.refreshUI()
        }
    }

    private func saveNetworkFilter(enabled: Bool) {
        if !isNetworkFilterExtensionInstalled() {
            presentError("network filter extension is missing from Cleanroom.app")
            refreshNetworkFilterStatus()
            return
        }

        loadNetworkFilterManager { [weak self] manager, error in
            guard let self else {
                return
            }
            if let error {
                self.presentError("failed to load network filter preferences: \(error.localizedDescription)")
                self.refreshNetworkFilterStatus()
                return
            }
            guard let manager else {
                self.presentError("failed to load network filter manager")
                self.refreshNetworkFilterStatus()
                return
            }

            if enabled {
                manager.providerConfiguration = self.makeNetworkFilterConfiguration()
                manager.localizedDescription = AppConstants.networkFilterDescription
            }
            manager.isEnabled = enabled
            manager.saveToPreferences { [weak self] saveError in
                DispatchQueue.main.async {
                    guard let self else {
                        return
                    }
                    if let saveError {
                        self.presentError("failed to \(enabled ? "enable" : "disable") network filter: \(saveError.localizedDescription)")
                    } else {
                        self.appendLog("\(enabled ? "enabled" : "disabled") network filter")
                        if enabled {
                            self.showInfo("Network filter enabled request submitted.\nmacOS may prompt for additional approval in System Settings.")
                        }
                    }
                    self.refreshNetworkFilterStatus()
                }
            }
        }
    }

    private func loadNetworkFilterManager(
        completion: @escaping (_ manager: NEFilterManager?, _ error: Error?) -> Void
    ) {
        let manager = NEFilterManager.shared()
        manager.loadFromPreferences { error in
            DispatchQueue.main.async {
                completion(manager, error)
            }
        }
    }

    private func makeNetworkFilterConfiguration() -> NEFilterProviderConfiguration {
        let configuration = NEFilterProviderConfiguration()
        configuration.filterSockets = true
        configuration.organization = AppConstants.networkFilterOrganization
        var vendorConfiguration: [String: Any] = [
            AppConstants.networkFilterPolicyPathVendorKey: networkFilterPolicyURL.path,
        ]
        if let helperURL = resolveDarwinVZHelperBinary() {
            vendorConfiguration[AppConstants.networkFilterTargetProcessVendorKey] = helperURL.path
        }
        configuration.vendorConfiguration = vendorConfiguration
        return configuration
    }

    private func isNetworkFilterExtensionInstalled() -> Bool {
        guard let pluginsURL = Bundle.main.builtInPlugInsURL else {
            return false
        }
        let extensionURL = pluginsURL.appendingPathComponent(
            AppConstants.networkFilterProviderBundleName,
            isDirectory: true
        )
        let infoURL = extensionURL.appendingPathComponent("Contents/Info.plist", isDirectory: false)
        let executableURL = extensionURL.appendingPathComponent(
            "Contents/MacOS/\(AppConstants.networkFilterProviderExecutableName)",
            isDirectory: false
        )
        let fm = FileManager.default
        return fm.fileExists(atPath: infoURL.path) && fm.isExecutableFile(atPath: executableURL.path)
    }

    private func isRunOnStartupEnabled() -> Bool {
        let defaults = UserDefaults.standard
        if defaults.object(forKey: AppConstants.runOnStartupPreferenceKey) == nil {
            return true
        }
        return defaults.bool(forKey: AppConstants.runOnStartupPreferenceKey)
    }

    private func setRunOnStartupEnabled(_ enabled: Bool) {
        UserDefaults.standard.set(enabled, forKey: AppConstants.runOnStartupPreferenceKey)
    }

    private func areRequiredCLISymlinksInstalled(cleanroomBinaryURL: URL?) -> Bool {
        guard let cleanroomBinaryURL else {
            return false
        }
        return isSymlink(linkPath: AppConstants.cliSymlinkPath, pointingTo: cleanroomBinaryURL.path)
    }

    private func isSymlink(linkPath: String, pointingTo targetPath: String) -> Bool {
        guard let destination = symlinkDestinationPath(linkPath: linkPath) else {
            return false
        }
        let expected = URL(fileURLWithPath: targetPath).resolvingSymlinksInPath().path
        let actual = URL(fileURLWithPath: destination).resolvingSymlinksInPath().path
        return expected == actual
    }

    private func symlinkDestinationPath(linkPath: String) -> String? {
        guard FileManager.default.fileExists(atPath: linkPath) else {
            return nil
        }
        guard let rawDest = try? FileManager.default.destinationOfSymbolicLink(atPath: linkPath) else {
            return nil
        }
        if rawDest.hasPrefix("/") {
            return rawDest
        }
        return URL(fileURLWithPath: linkPath)
            .deletingLastPathComponent()
            .appendingPathComponent(rawDest)
            .path
    }

    private func installRequiredCLISymlinksWithAdmin(cleanroomBinaryURL: URL) throws {
        var shellCommand = "/bin/mkdir -p \(shellQuote(AppConstants.cliBinDir))"
        shellCommand += " && /bin/rm -f \(shellQuote(AppConstants.cliSymlinkPath))"
        shellCommand += " && /bin/ln -s \(shellQuote(cleanroomBinaryURL.path)) \(shellQuote(AppConstants.cliSymlinkPath))"

        let appleScript = "do shell script \"\(escapeForAppleScript(shellCommand))\" with administrator privileges"
        let result = try runCommandAllowFailure("/usr/bin/osascript", ["-e", appleScript])
        if result.terminationStatus != 0 {
            throw AppError.commandFailed(
                command: "install CLI symlink",
                details: commandDetails(result)
            )
        }
    }

    private func resolveCleanroomBinary() -> URL? {
        let env = ProcessInfo.processInfo.environment
        if let override = env[AppConstants.cleanroomBinaryOverrideEnv], !override.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            let path = override.trimmingCharacters(in: .whitespacesAndNewlines)
            if FileManager.default.isExecutableFile(atPath: path) {
                return URL(fileURLWithPath: path)
            }
        }

        let bundled = Bundle.main.bundleURL
            .appendingPathComponent("Contents/Helpers/cleanroom", isDirectory: false)
            .path
        if FileManager.default.isExecutableFile(atPath: bundled) {
            return URL(fileURLWithPath: bundled)
        }

        if let pathValue = env["PATH"] {
            for entry in pathValue.split(separator: ":") {
                if entry.isEmpty {
                    continue
                }
                let candidate = String(entry) + "/cleanroom"
                if FileManager.default.isExecutableFile(atPath: candidate) {
                    return URL(fileURLWithPath: candidate)
                }
            }
        }

        let fallback = "/usr/local/bin/cleanroom"
        if FileManager.default.isExecutableFile(atPath: fallback) {
            return URL(fileURLWithPath: fallback)
        }
        return nil
    }

    private func resolveDarwinVZHelperBinary() -> URL? {
        let bundled = Bundle.main.bundleURL
            .appendingPathComponent("Contents/Helpers/cleanroom-darwin-vz", isDirectory: false)
            .path
        if FileManager.default.isExecutableFile(atPath: bundled) {
            return URL(fileURLWithPath: bundled)
        }

        if let cleanroom = resolvedBinaryURL {
            let candidate = cleanroom.deletingLastPathComponent().appendingPathComponent("cleanroom-darwin-vz")
            if FileManager.default.isExecutableFile(atPath: candidate.path) {
                return candidate
            }
        }
        return nil
    }

    private func openAppLogHandle() throws -> FileHandle {
        if let existing = appLogHandle {
            return existing
        }

        let dirURL = appLogURL.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: dirURL, withIntermediateDirectories: true)
        if !FileManager.default.fileExists(atPath: appLogURL.path) {
            FileManager.default.createFile(atPath: appLogURL.path, contents: Data())
        }
        let handle = try FileHandle(forWritingTo: appLogURL)
        try handle.seekToEnd()
        appLogHandle = handle
        return handle
    }

    private func closeAppLogHandle() {
        guard let handle = appLogHandle else {
            return
        }
        do {
            try handle.close()
        } catch {
            // Best-effort close.
        }
        appLogHandle = nil
    }

    private func appendLog(_ message: String) {
        guard let handle = try? openAppLogHandle() else {
            return
        }
        let timestamp = ISO8601DateFormatter().string(from: Date())
        let line = "[\(timestamp)] \(message)\n"
        if let data = line.data(using: .utf8) {
            do {
                try handle.write(contentsOf: data)
            } catch {
                // Best-effort log append.
            }
        }
    }

    private func presentError(_ message: String) {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = AppConstants.menuTitle
        alert.informativeText = message
        alert.runModal()
    }

    private func showInfo(_ message: String) {
        let alert = NSAlert()
        alert.alertStyle = .informational
        alert.messageText = AppConstants.menuTitle
        alert.informativeText = message
        alert.runModal()
    }
}

private func shellQuote(_ value: String) -> String {
    if value.isEmpty {
        return "''"
    }
    let escaped = value.replacingOccurrences(of: "'", with: "'\"'\"'")
    return "'\(escaped)'"
}

private func escapeForAppleScript(_ value: String) -> String {
    let escapedSlashes = value.replacingOccurrences(of: "\\", with: "\\\\")
    return escapedSlashes.replacingOccurrences(of: "\"", with: "\\\"")
}

private func readAllString(from handle: FileHandle) -> String {
    let data = (try? handle.readToEnd()) ?? Data()
    return String(data: data, encoding: .utf8)?
        .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
}

let appDelegate = CleanroomMenuBarApp()
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
app.delegate = appDelegate
app.run()

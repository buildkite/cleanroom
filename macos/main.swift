import AppKit
import Foundation
import NetworkExtension
import SystemExtensions

private enum AppConstants {
    static let menuTitle = "Cleanroom"
    static let networkFilterDescription = "Cleanroom Network Filter"
    static let networkFilterOrganization = "Buildkite Cleanroom"
    static let networkFilterProviderBundleIdentifier = "com.buildkite.cleanroom.network.filter"
    static let networkFilterProviderBundleName = "com.buildkite.cleanroom.network.filter.systemextension"
    static let networkFilterProviderExecutableName = "CleanroomFilterDataProvider"
    static let networkFilterDaemonBaseURL = NetworkFilterDaemonClient.defaultBaseURL
    static let networkFilterDaemonInstallCommand = "cleanroom network install"
    static let networkFilterProviderActivationTimeoutSeconds: TimeInterval = 5
    static let networkFilterProviderActivationPollIntervalSeconds: TimeInterval = 0.25
    static let appLogFilename = "cleanroom-network.log"
    static let manualLaunchMessage =
        "Cleanroom.app is a support app for Cleanroom network filtering.\n\n" +
        "Use the CLI instead:\n" +
        "cleanroom network install\n" +
        "cleanroom network enable\n" +
        "cleanroom network disable\n" +
        "cleanroom network reset\n" +
        "cleanroom network status"
}

private enum AppError: LocalizedError {
    case networkFilterManagerUnavailable
    case systemExtensionActivationInProgress
    case systemExtensionInstallRejected

    var errorDescription: String? {
        switch self {
        case .networkFilterManagerUnavailable:
            return "network filter manager unavailable"
        case .systemExtensionActivationInProgress:
            return "system extension activation already in progress"
        case .systemExtensionInstallRejected:
            return "system extension activation was rejected by macOS"
        }
    }
}

private enum AppInvocationCommand: String {
    case enable
    case disable
    case reset
    case status
}

private struct AppInvocation {
    let command: AppInvocationCommand?
    let json: Bool

    var isHeadless: Bool {
        command != nil
    }

    static func parse(arguments: [String]) -> AppInvocation {
        var command: AppInvocationCommand?
        var json = false
        var index = 1

        while index < arguments.count {
            let argument = arguments[index]
            switch argument {
            case "--network-command":
                let nextIndex = index + 1
                if nextIndex < arguments.count {
                    command = AppInvocationCommand(rawValue: arguments[nextIndex])
                    index = nextIndex
                }
            case "--json":
                json = true
            default:
                break
            }
            index += 1
        }

        return AppInvocation(command: command, json: json)
    }
}

private struct NetworkFilterAppStatusSnapshot: Encodable {
    let appBundlePath: String
    let extensionInstalled: Bool
    let daemonHealthy: Bool
    let available: Bool
    let loaded: Bool
    let configured: Bool
    let enabled: Bool
    let lastError: String?
    let providerValidation: String?

    private enum CodingKeys: String, CodingKey {
        case appBundlePath = "app_bundle_path"
        case extensionInstalled = "extension_installed"
        case daemonHealthy = "daemon_healthy"
        case available
        case loaded
        case configured
        case enabled
        case lastError = "last_error"
        case providerValidation = "provider_validation"
    }
}

final class CleanroomSupportApp: NSObject, NSApplicationDelegate, OSSystemExtensionRequestDelegate {
    private let invocation: AppInvocation

    private var appLogHandle: FileHandle?
    private var networkFilterAvailable = false
    private var networkFilterLoaded = false
    private var networkFilterEnabled = false
    private var networkFilterConfigured = false
    private var networkFilterLastError: String?
    private var systemExtensionRequestCompletion: ((Error?) -> Void)?
    private var pendingNetworkFilterEnable = false
    private(set) var exitCode: Int32 = 0

    private lazy var appSupportDirectoryURL: URL = resolveAppSupportDirectoryURL()

    private lazy var appLogURL: URL = {
        appSupportDirectoryURL.appendingPathComponent(AppConstants.appLogFilename, isDirectory: false)
    }()

    fileprivate init(invocation: AppInvocation) {
        self.invocation = invocation
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        if invocation.isHeadless {
            runHeadlessCommand()
            return
        }
        showInfo(AppConstants.manualLaunchMessage)
        exitCode = 0
        NSApp.terminate(nil)
    }

    func applicationWillTerminate(_ notification: Notification) {
        closeAppLogHandle()
    }

    private func resolveAppSupportDirectoryURL() -> URL {
        let fm = FileManager.default
        if let appSupportURL = fm.urls(for: .applicationSupportDirectory, in: .userDomainMask).first {
            return appSupportURL.appendingPathComponent(AppConstants.menuTitle, isDirectory: true)
        }
        return fm.homeDirectoryForCurrentUser
            .appendingPathComponent("Library", isDirectory: true)
            .appendingPathComponent("Application Support", isDirectory: true)
            .appendingPathComponent(AppConstants.menuTitle, isDirectory: true)
    }

    private func persistNetworkFilterStatusSnapshot() {
        let payload: [String: Any] = {
            var value: [String: Any] = [
                "version": 1,
                "available": networkFilterAvailable,
                "loaded": networkFilterLoaded,
                "configured": networkFilterConfigured,
                "enabled": networkFilterEnabled,
            ]
            if let lastError = networkFilterLastError?.trimmingCharacters(in: .whitespacesAndNewlines),
               !lastError.isEmpty
            {
                value["last_error"] = lastError
            } else {
                value["last_error"] = NSNull()
            }
            return value
        }()
        do {
            _ = try networkFilterDaemonClient().patchStatus(payload)
        } catch {
            appendLog("failed to persist network filter status: \(error.localizedDescription)")
        }
    }

    private func networkFilterDaemonClient() throws -> NetworkFilterDaemonClient {
        try NetworkFilterDaemonClient(baseURLString: AppConstants.networkFilterDaemonBaseURL)
    }

    private func fetchNetworkFilterDaemonStatus() throws -> NetworkFilterDaemonStatusSnapshot? {
        try networkFilterDaemonClient().getStatus()
    }

    private func networkFilterDaemonHealthy() -> Bool {
        do {
            try networkFilterDaemonClient().healthCheck()
            return true
        } catch {
            return false
        }
    }

    private func refreshNetworkFilterStatus() {
        if !isNetworkFilterExtensionInstalled() {
            networkFilterAvailable = false
            networkFilterLoaded = false
            networkFilterConfigured = false
            networkFilterEnabled = false
            networkFilterLastError = "filter extension is not bundled in Cleanroom.app"
            persistNetworkFilterStatusSnapshot()
            return
        }

        networkFilterAvailable = true
        loadNetworkFilterManager { [weak self] manager, error in
            guard let self else {
                return
            }
            self.updateNetworkFilterStatus(manager: manager, error: error)
        }
    }

    private func setNetworkFilterEnabled(
        _ enabled: Bool,
        completion: @escaping (Result<String, Error>) -> Void
    ) {
        if !isNetworkFilterExtensionInstalled() {
            completion(.failure(NSError(
                domain: AppConstants.menuTitle,
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "network filter system extension is missing from Cleanroom.app"]
            )))
            refreshNetworkFilterStatus()
            return
        }

        if enabled {
            ensureNetworkFilterDaemonReady { [weak self] daemonError in
                guard let self else {
                    return
                }
                if let daemonError {
                    completion(.failure(NSError(
                        domain: AppConstants.menuTitle,
                        code: 1,
                        userInfo: [NSLocalizedDescriptionKey:
                            "failed to install or start the Cleanroom network daemon: \(daemonError.localizedDescription)"
                        ]
                    )))
                    self.refreshNetworkFilterStatus()
                    return
                }
                self.activateSystemExtensionIfNeeded { [weak self] error in
                    guard let self else {
                        return
                    }
                    if let error {
                        completion(.failure(NSError(
                            domain: AppConstants.menuTitle,
                            code: 1,
                            userInfo: [NSLocalizedDescriptionKey:
                                "failed to activate Cleanroom network system extension: \(error.localizedDescription)"
                            ]
                        )))
                        self.refreshNetworkFilterStatus()
                        return
                    }
                    self.saveLoadedNetworkFilterPreferences(enabled: true, completion: completion)
                }
            }
            return
        }

        saveLoadedNetworkFilterPreferences(enabled: false, completion: completion)
    }

    private func saveLoadedNetworkFilterPreferences(
        enabled: Bool,
        completion: @escaping (Result<String, Error>) -> Void
    ) {
        loadNetworkFilterManager { [weak self] manager, error in
            guard let self else {
                return
            }
            if let error {
                completion(.failure(NSError(
                    domain: AppConstants.menuTitle,
                    code: 1,
                    userInfo: [NSLocalizedDescriptionKey:
                        "failed to load network filter preferences: \(error.localizedDescription)"
                    ]
                )))
                self.refreshNetworkFilterStatus()
                return
            }
            guard let manager else {
                completion(.failure(NSError(
                    domain: AppConstants.menuTitle,
                    code: 1,
                    userInfo: [NSLocalizedDescriptionKey: "failed to load network filter manager"]
                )))
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
                        completion(.failure(NSError(
                            domain: AppConstants.menuTitle,
                            code: 1,
                            userInfo: [NSLocalizedDescriptionKey:
                                "failed to \(enabled ? "enable" : "disable") network filter: \(saveError.localizedDescription)"
                            ]
                        )))
                        self.refreshNetworkFilterStatus()
                        return
                    }

                    self.appendLog("\(enabled ? "enabled" : "disabled") network filter")
                    let activationStartedAt = Date()
                    self.loadNetworkFilterManager { [weak self] refreshedManager, loadError in
                        guard let self else {
                            return
                        }
                        self.updateNetworkFilterStatus(manager: refreshedManager, error: loadError)
                        if enabled {
                            if self.networkFilterEnabled {
                                self.waitForNetworkFilterProviderActivation(since: activationStartedAt) { [weak self] activationError in
                                    guard let self else {
                                        return
                                    }
                                    self.networkFilterLastError = activationError
                                    self.persistNetworkFilterStatusSnapshot()
                                    if let activationError {
                                        self.appendLog("network filter provider validation failed: \(activationError)")
                                        completion(.failure(NSError(
                                            domain: AppConstants.menuTitle,
                                            code: 1,
                                            userInfo: [NSLocalizedDescriptionKey:
                                                "Network filter enabled in preferences, but the provider has not started.\n" +
                                                "\(activationError)"
                                            ]
                                        )))
                                    } else {
                                        completion(.success("Network filter enabled."))
                                    }
                                }
                                return
                            }
                            completion(.success("Network filter enable request submitted.\nApprove it in System Settings if prompted."))
                            return
                        }

                        completion(.success("Network filter disabled."))
                    }
                }
            }
        }
    }

    private func updateNetworkFilterStatus(manager: NEFilterManager?, error: Error?) {
        networkFilterAvailable = isNetworkFilterExtensionInstalled()
        if let error {
            networkFilterLoaded = false
            networkFilterConfigured = false
            networkFilterEnabled = false
            networkFilterLastError = error.localizedDescription
        } else {
            networkFilterLoaded = true
            networkFilterConfigured = manager?.providerConfiguration != nil
            networkFilterEnabled = manager?.isEnabled ?? false
            networkFilterLastError = networkFilterEnabled ? currentProviderValidationDetail() : nil
        }
        persistNetworkFilterStatusSnapshot()
    }

    private func removeNetworkFilterPreferences(completion: @escaping (Error?) -> Void) {
        guard isNetworkFilterExtensionInstalled() else {
            completion(nil)
            return
        }

        loadNetworkFilterManager { manager, error in
            if let error {
                completion(error)
                return
            }
            guard let manager else {
                completion(AppError.networkFilterManagerUnavailable)
                return
            }
            manager.isEnabled = false
            manager.removeFromPreferences { removeError in
                DispatchQueue.main.async {
                    completion(removeError)
                }
            }
        }
    }

    private func performResetFilterState(completion: @escaping (Result<String, Error>) -> Void) {
        removeNetworkFilterPreferences { [weak self] filterError in
            guard let self else {
                return
            }

            var issues = [String]()
            if let filterError {
                issues.append("network filter preferences: \(filterError.localizedDescription)")
            }

            do {
                try self.resetNetworkFilterDaemonState()
            } catch {
                issues.append("network filter daemon state: \(error.localizedDescription)")
            }

            self.appendLog("reset network filter state")
            self.refreshNetworkFilterStatus()

            if issues.isEmpty {
                completion(.success("Network filter preferences removed and daemon state reset."))
                return
            }

            completion(.failure(NSError(
                domain: AppConstants.menuTitle,
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "Reset completed with warnings:\n" + issues.joined(separator: "\n")]
            )))
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
        configuration.filterPackets = true
        configuration.filterSockets = false
        configuration.filterPacketProviderBundleIdentifier = AppConstants.networkFilterProviderBundleIdentifier
        configuration.organization = AppConstants.networkFilterOrganization
        return configuration
    }

    private func currentProviderValidationDetail(since activationStartedAt: Date? = nil) -> String? {
        guard networkFilterEnabled else {
            return nil
        }

        let status: NetworkFilterDaemonStatusSnapshot
        do {
            guard let snapshot = try fetchNetworkFilterDaemonStatus() else {
                return "network filter daemon status is unavailable"
            }
            status = snapshot
        } catch {
            return "network filter daemon is unavailable: \(error.localizedDescription)"
        }

        if let providerLastError = status.providerLastError?.trimmingCharacters(in: .whitespacesAndNewlines),
           !providerLastError.isEmpty
        {
            return providerLastError
        }

        if let activationStartedAt {
            if let providerStartedAt = parseNetworkFilterTimestamp(status.providerStartedAt),
               providerStartedAt >= activationStartedAt {
                return nil
            }
            if let providerUpdatedAt = parseNetworkFilterTimestamp(status.providerUpdatedAt),
               providerUpdatedAt >= activationStartedAt
            {
                return nil
            }
            return "network filter provider has not started"
        }

        if let providerStartedAt = status.providerStartedAt,
           !providerStartedAt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            return nil
        }
        return "network filter provider has not started"
    }

    private func waitForNetworkFilterProviderActivation(
        since activationStartedAt: Date,
        completion: @escaping (String?) -> Void
    ) {
        let deadline = Date().addingTimeInterval(AppConstants.networkFilterProviderActivationTimeoutSeconds)

        func poll() {
            let detail = self.currentProviderValidationDetail(since: activationStartedAt)
            if detail == nil {
                completion(nil)
                return
            }
            if Date() >= deadline {
                completion(detail)
                return
            }
            DispatchQueue.main.asyncAfter(
                deadline: .now() + AppConstants.networkFilterProviderActivationPollIntervalSeconds
            ) {
                poll()
            }
        }

        poll()
    }

    private func ensureNetworkFilterDaemonReady(completion: @escaping (Error?) -> Void) {
        if networkFilterDaemonHealthy() {
            completion(nil)
            return
        }

        let message =
            "network filter daemon is unavailable. Run `\(AppConstants.networkFilterDaemonInstallCommand)` in Terminal, then retry."
        appendLog(message)
        completion(NSError(
            domain: AppConstants.menuTitle,
            code: 1,
            userInfo: [NSLocalizedDescriptionKey: message]
        ))
    }

    private func parseNetworkFilterTimestamp(_ raw: String?) -> Date? {
        guard let raw else {
            return nil
        }
        let value = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !value.isEmpty else {
            return nil
        }

        let formatter = ISO8601DateFormatter()
        if let parsed = formatter.date(from: value) {
            return parsed
        }
        formatter.formatOptions = [.withInternetDateTime]
        return formatter.date(from: value)
    }

    private func isNetworkFilterExtensionInstalled() -> Bool {
        let extensionURL = Bundle.main.bundleURL
            .appendingPathComponent("Contents/Library/SystemExtensions", isDirectory: true)
            .appendingPathComponent(
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

    private func activateSystemExtensionIfNeeded(completion: @escaping (Error?) -> Void) {
        guard isNetworkFilterExtensionInstalled() else {
            completion(AppError.systemExtensionInstallRejected)
            return
        }
        guard systemExtensionRequestCompletion == nil else {
            completion(AppError.systemExtensionActivationInProgress)
            return
        }

        pendingNetworkFilterEnable = true
        systemExtensionRequestCompletion = completion

        let request = OSSystemExtensionRequest.activationRequest(
            forExtensionWithIdentifier: AppConstants.networkFilterProviderBundleIdentifier,
            queue: .main
        )
        request.delegate = self
        appendLog("submitted system extension activation request for \(AppConstants.networkFilterProviderBundleIdentifier)")
        OSSystemExtensionManager.shared.submitRequest(request)
    }

    func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
        appendLog("system extension activation requires user approval")
        showInfo(
            "Approve the Cleanroom network system extension in System Settings.\n" +
            "Cleanroom will continue enabling the network filter once macOS finishes the activation request."
        )
    }

    func request(_ request: OSSystemExtensionRequest, didFailWithError error: Error) {
        appendLog("system extension activation failed: \(error.localizedDescription)")
        let completion = systemExtensionRequestCompletion
        systemExtensionRequestCompletion = nil
        pendingNetworkFilterEnable = false
        completion?(error)
    }

    func request(_ request: OSSystemExtensionRequest, didFinishWithResult result: OSSystemExtensionRequest.Result) {
        appendLog("system extension activation finished with result \(result.rawValue)")
        let completion = systemExtensionRequestCompletion
        systemExtensionRequestCompletion = nil
        let shouldEnableFilter = pendingNetworkFilterEnable
        pendingNetworkFilterEnable = false
        guard shouldEnableFilter else {
            completion?(nil)
            return
        }
        completion?(nil)
    }

    func request(
        _ request: OSSystemExtensionRequest,
        actionForReplacingExtension existing: OSSystemExtensionProperties,
        withExtension ext: OSSystemExtensionProperties
    ) -> OSSystemExtensionRequest.ReplacementAction {
        appendLog("replacing existing system extension version \(existing.bundleVersion) with \(ext.bundleVersion)")
        return .replace
    }

    private func resetNetworkFilterDaemonState() throws {
        do {
            try networkFilterDaemonClient().reset()
        } catch {
            if networkFilterDaemonHealthy() {
                throw error
            }
        }
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

    private func runHeadlessCommand() {
        guard let command = invocation.command else {
            completeHeadlessFailure("missing network command")
            return
        }

        switch command {
        case .enable:
            setNetworkFilterEnabled(true) { [weak self] result in
                self?.completeHeadless(result)
            }
        case .disable:
            setNetworkFilterEnabled(false) { [weak self] result in
                self?.completeHeadless(result)
            }
        case .reset:
            performResetFilterState { [weak self] result in
                self?.completeHeadless(result)
            }
        case .status:
            emitHeadlessStatus()
        }
    }

    private func emitHeadlessStatus() {
        refreshNetworkFilterStatus()
        loadNetworkFilterManager { [weak self] manager, error in
            guard let self else {
                return
            }
            self.updateNetworkFilterStatus(manager: manager, error: error)
            let snapshot = NetworkFilterAppStatusSnapshot(
                appBundlePath: Bundle.main.bundleURL.path,
                extensionInstalled: self.isNetworkFilterExtensionInstalled(),
                daemonHealthy: self.networkFilterDaemonHealthy(),
                available: self.networkFilterAvailable,
                loaded: self.networkFilterLoaded,
                configured: self.networkFilterConfigured,
                enabled: self.networkFilterEnabled,
                lastError: self.networkFilterLastError,
                providerValidation: self.currentProviderValidationDetail()
            )

            do {
                let encoder = JSONEncoder()
                encoder.outputFormatting = self.invocation.json ? [.prettyPrinted, .sortedKeys] : [.sortedKeys]
                let data = try encoder.encode(snapshot)
                FileHandle.standardOutput.write(data)
                FileHandle.standardOutput.write(Data("\n".utf8))
                self.exitCode = 0
                NSApp.terminate(nil)
            } catch {
                self.completeHeadlessFailure("failed to encode status output: \(error.localizedDescription)")
            }
        }
    }

    private func completeHeadless(_ result: Result<String, Error>) {
        switch result {
        case .success(let message):
            completeHeadlessSuccess(message)
        case .failure(let error):
            completeHeadlessFailure(error.localizedDescription)
        }
    }

    private func completeHeadlessSuccess(_ message: String) {
        if !message.isEmpty {
            FileHandle.standardOutput.write(Data((message + "\n").utf8))
        }
        exitCode = 0
        NSApp.terminate(nil)
    }

    private func completeHeadlessFailure(_ message: String) {
        if !message.isEmpty {
            FileHandle.standardError.write(Data((message + "\n").utf8))
        }
        exitCode = 1
        NSApp.terminate(nil)
    }

    private func presentError(_ message: String) {
        if invocation.isHeadless {
            appendLog(message)
            FileHandle.standardError.write(Data((message + "\n").utf8))
            return
        }
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = AppConstants.menuTitle
        alert.informativeText = message
        alert.runModal()
    }

    private func showInfo(_ message: String) {
        if invocation.isHeadless {
            appendLog(message)
            FileHandle.standardError.write(Data((message + "\n").utf8))
            return
        }
        let alert = NSAlert()
        alert.alertStyle = .informational
        alert.messageText = AppConstants.menuTitle
        alert.informativeText = message
        alert.runModal()
    }
}

private let appInvocation = AppInvocation.parse(arguments: CommandLine.arguments)
private let appDelegate = CleanroomSupportApp(invocation: appInvocation)
private let app = NSApplication.shared
app.setActivationPolicy(appInvocation.isHeadless ? .prohibited : .accessory)
app.delegate = appDelegate
app.run()
exit(appDelegate.exitCode)

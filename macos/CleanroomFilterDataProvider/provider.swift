import Darwin
import Foundation
import Network
import NetworkExtension

@objc(CleanroomFilterDataProvider)
final class CleanroomFilterDataProvider: NEFilterDataProvider {
    private struct PolicySnapshot: Decodable {
        let version: Int
        let updatedAt: String
        let defaultAction: String
        let targetProcessPath: String?
        let allow: [AllowRule]
        let processRules: [ProcessRule]?

        private enum CodingKeys: String, CodingKey {
            case version
            case updatedAt = "updated_at"
            case defaultAction = "default_action"
            case targetProcessPath = "target_process_path"
            case allow
            case processRules = "process_rules"
        }
    }

    private struct AllowRule: Decodable {
        let host: String
        let ports: [Int]
    }

    private struct ProcessRule: Decodable {
        let pid: Int32
        let allow: [AllowRule]
    }

    private enum ProviderConstants {
        static let policyPathVendorKey = "policy_path"
        static let targetProcessVendorKey = "target_process_path"
        static let defaultActionDeny = "deny"
    }

    private let decoder = JSONDecoder()
    private let cacheLock = NSLock()
    private var cachedPolicyPath = ""
    private var cachedPolicyMTime: Date?
    private var cachedPolicy: PolicySnapshot?

    override func startFilter(completionHandler: @escaping (Error?) -> Void) {
        _ = loadPolicySnapshot()
        completionHandler(nil)
    }

    override func stopFilter(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        completionHandler()
    }

    override func handleNewFlow(_ flow: NEFilterFlow) -> NEFilterNewFlowVerdict {
        guard shouldEvaluate(flow) else {
            return .allow()
        }

        guard let policy = loadPolicySnapshot() else {
            return .drop()
        }
        guard policy.defaultAction.trimmingCharacters(in: .whitespacesAndNewlines).lowercased() == ProviderConstants.defaultActionDeny else {
            return .allow()
        }
        guard let (host, port) = remoteHostPort(for: flow) else {
            return .drop()
        }

        if let pid = sourceProcessPID(for: flow), let scopedAllow = scopedAllowRules(for: pid, policy: policy) {
            if isAllowed(host: host, port: port, rules: scopedAllow) {
                return .allow()
            }
            return .drop()
        }

        if let processRules = policy.processRules, !processRules.isEmpty {
            return .drop()
        }

        if isAllowed(host: host, port: port, rules: policy.allow) {
            return .allow()
        }
        return .drop()
    }

    private func shouldEvaluate(_ flow: NEFilterFlow) -> Bool {
        let configuredTarget = configuredTargetProcessPath()
        if configuredTarget.isEmpty {
            return false
        }
        guard let sourcePath = sourceProcessPath(for: flow) else {
            return false
        }
        return canonicalPath(sourcePath) == canonicalPath(configuredTarget)
    }

    private func configuredPolicyPath() -> String {
        guard let raw = filterConfiguration.vendorConfiguration?[ProviderConstants.policyPathVendorKey] as? String else {
            return ""
        }
        return raw.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func configuredTargetProcessPath() -> String {
        guard let raw = filterConfiguration.vendorConfiguration?[ProviderConstants.targetProcessVendorKey] as? String else {
            return ""
        }
        return raw.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func loadPolicySnapshot() -> PolicySnapshot? {
        let path = configuredPolicyPath()
        if path.isEmpty {
            return nil
        }

        let mtime = (try? FileManager.default.attributesOfItem(atPath: path)[.modificationDate]) as? Date

        cacheLock.lock()
        if path == cachedPolicyPath, mtime == cachedPolicyMTime, let cachedPolicy {
            cacheLock.unlock()
            return cachedPolicy
        }
        cacheLock.unlock()

        guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)) else {
            cacheLock.lock()
            cachedPolicyPath = path
            cachedPolicyMTime = mtime
            cachedPolicy = nil
            cacheLock.unlock()
            return nil
        }
        guard let decoded = try? decoder.decode(PolicySnapshot.self, from: data) else {
            cacheLock.lock()
            cachedPolicyPath = path
            cachedPolicyMTime = mtime
            cachedPolicy = nil
            cacheLock.unlock()
            return nil
        }

        cacheLock.lock()
        cachedPolicyPath = path
        cachedPolicyMTime = mtime
        cachedPolicy = decoded
        cacheLock.unlock()
        return decoded
    }

    private func remoteHostPort(for flow: NEFilterFlow) -> (String, Int)? {
        guard let socketFlow = flow as? NEFilterSocketFlow else {
            return nil
        }
        if #available(macOS 15.0, *) {
            guard let endpoint = socketFlow.remoteFlowEndpoint else {
                return nil
            }
            guard case let .hostPort(host, port) = endpoint else {
                return nil
            }
            let hostValue = normalizeHost(String(describing: host))
            guard !hostValue.isEmpty else {
                return nil
            }
            let portValue = Int(port.rawValue)
            guard portValue >= 1, portValue <= 65535 else {
                return nil
            }
            return (hostValue, portValue)
        }
        // Use KVC for pre-macOS 15 endpoint access to avoid hard references to
        // deprecated NetworkExtension endpoint symbols in current SDKs.
        guard
            let endpointObject = (socketFlow as NSObject).value(forKey: "remoteEndpoint") as? NSObject,
            let rawHost = endpointObject.value(forKey: "hostname") as? String
        else {
            return nil
        }
        let hostValue = normalizeHost(rawHost)
        guard !hostValue.isEmpty else {
            return nil
        }
        guard
            let rawPort = endpointObject.value(forKey: "port") as? String,
            let portValue = Int(rawPort),
            portValue >= 1,
            portValue <= 65535
        else {
            return nil
        }
        return (hostValue, portValue)
    }

    private func sourceProcessPath(for flow: NEFilterFlow) -> String? {
        guard let pid = sourceProcessPID(for: flow), pid > 0 else {
            return nil
        }

        var buffer = [CChar](repeating: 0, count: Int(MAXPATHLEN))
        let length = proc_pidpath(pid, &buffer, UInt32(buffer.count))
        guard length > 0 else {
            return nil
        }
        return String(cString: buffer)
    }

    private func normalizeHost(_ host: String) -> String {
        host
            .trimmingCharacters(in: .whitespacesAndNewlines)
            .trimmingCharacters(in: CharacterSet(charactersIn: "."))
            .lowercased()
    }

    private func sourceProcessPID(for flow: NEFilterFlow) -> Int32? {
        guard let tokenData = flow.sourceProcessAuditToken else {
            return nil
        }
        let pid: Int32 = tokenData.withUnsafeBytes { raw in
            guard raw.count >= MemoryLayout<audit_token_t>.size else {
                return -1
            }
            guard let token = raw.bindMemory(to: audit_token_t.self).baseAddress?.pointee else {
                return -1
            }
            return audit_token_to_pid(token)
        }
        if pid <= 0 {
            return nil
        }
        return pid
    }

    private func scopedAllowRules(for pid: Int32, policy: PolicySnapshot) -> [AllowRule]? {
        guard let processRules = policy.processRules else {
            return nil
        }
        for rule in processRules where rule.pid == pid {
            return rule.allow
        }
        return nil
    }

    private func canonicalPath(_ value: String) -> String {
        URL(fileURLWithPath: value)
            .resolvingSymlinksInPath()
            .standardizedFileURL
            .path
    }

    private func isAllowed(host: String, port: Int, rules: [AllowRule]) -> Bool {
        for rule in rules {
            let ruleHost = rule.host.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
            guard !ruleHost.isEmpty else {
                continue
            }
            if !hostMatches(host: host, ruleHost: ruleHost) {
                continue
            }
            if rule.ports.contains(port) {
                return true
            }
        }
        return false
    }

    private func hostMatches(host: String, ruleHost: String) -> Bool {
        if ruleHost == host {
            return true
        }
        if ruleHost.hasPrefix("*.") {
            let suffix = String(ruleHost.dropFirst(1))
            return host.hasSuffix(suffix)
        }
        if ruleHost.hasPrefix(".") {
            return host.hasSuffix(ruleHost)
        }
        return false
    }
}

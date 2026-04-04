import Darwin
import Foundation
import Network
import NetworkExtension
import OSLog

@objc(CleanroomFilterDataProvider)
final class CleanroomFilterDataProvider: NEFilterPacketProvider {
    private enum ProviderConstants {
        static let heartbeatIntervalSeconds: TimeInterval = 30
        static let policyCacheTTLSeconds: TimeInterval = 1
        static let packetStatusThrottleSeconds: TimeInterval = 1
        static let packetFlushIntervalSeconds: TimeInterval = 2
        static let packetTraceLimit = 24
        static let dnsPort = 53
    }

    private struct PolicyLoadResult {
        let policy: NetworkFilterDaemonPolicySnapshot?
        let errorDetail: String?
    }

    private struct PacketSummary {
        let interfaceName: String
        let direction: String
        let length: Int
        let etherType: String?
        let sourceIP: String?
        let destinationIP: String?
        let ipProtocol: UInt8?
        let sourcePort: Int?
        let destinationPort: Int?
        let verdict: String
        let reason: String
        let guestIP: String?
        let matchRole: String
    }

    private struct PendingPacketObservation {
        let summary: PacketSummary
        let packetCount: Int
        let observedAt: Date
    }

    private struct TracePacketSample {
        let fingerprint: String
        let interfaceName: String
        let direction: String
        let length: Int
        let etherType: String?
        let sourceIP: String?
        let destinationIP: String?
        let ipProtocol: UInt8?
        let sourcePort: Int?
        let destinationPort: Int?
        let verdict: String
        let reason: String
        let guestIP: String?
        let matchRole: String
        var count: Int
        var lastObservedAt: Date

        func statusPayload() -> [String: Any] {
            var payload: [String: Any] = [
                "interface": interfaceName,
                "direction": direction,
                "length": length,
                "verdict": verdict,
                "reason": reason,
                "match_role": matchRole,
                "count": count,
                "last_seen_at": ISO8601DateFormatter().string(from: lastObservedAt),
            ]
            if let etherType {
                payload["ether_type"] = etherType
            }
            if let sourceIP {
                payload["source_ip"] = sourceIP
            }
            if let destinationIP {
                payload["destination_ip"] = destinationIP
            }
            if let ipProtocol {
                payload["ip_protocol"] = Int(ipProtocol)
            }
            if let sourcePort {
                payload["source_port"] = sourcePort
            }
            if let destinationPort {
                payload["destination_port"] = destinationPort
            }
            if let guestIP {
                payload["guest_ip"] = guestIP
            }
            return payload
        }
    }

    private let cacheLock = NSLock()
    private let packetObservationLock = NSLock()
    private let logger = Logger(subsystem: "com.buildkite.cleanroom.network.filter", category: "provider")
    private let daemonClient = try? NetworkFilterDaemonClient()
    private var cachedPolicyFetchedAt: Date?
    private var cachedPolicy: NetworkFilterDaemonPolicySnapshot?
    private var heartbeatTimer: DispatchSourceTimer?
    private var packetFlushTimer: DispatchSourceTimer?
    private var lastPacketStatusAt: Date?
    private var pendingPacketObservation: PendingPacketObservation?
    private var traceSamples: [TracePacketSample] = []
    private var traceDirty = false
    private var packetsSeen = 0

    override init() {
        super.init()
        logger.fault("provider init")
    }

    override func startFilter(completionHandler: @escaping (Error?) -> Void) {
        logger.fault("startFilter invoked")
        let loadResult = loadPolicySnapshot()

        guard #available(macOS 15.0, *) else {
            let error = NSError(
                domain: "Cleanroom",
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "NEFilterPacketProvider handler requires macOS 15 or later"]
            )
            writeProviderStatus { payload in
                let now = iso8601Now()
                payload["provider_updated_at"] = now
                payload["provider_started_at"] = now
                payload["provider_last_event"] = "start"
                payload["provider_last_error"] = error.localizedDescription
            }
            completionHandler(error)
            return
        }

        handler = { [weak self] _, interface, direction, packet in
            guard let self else {
                return .allow
            }
            let allowed = self.observePacket(interface: interface, direction: direction, packet: packet)
            return allowed ? .allow : .drop
        }

        startHeartbeat()
        startPacketFlushTimer()
        writeProviderStatus { payload in
            let now = iso8601Now()
            payload["provider_updated_at"] = now
            payload["provider_started_at"] = now
            payload["provider_last_event"] = "start"
            payload["provider_last_error"] = loadResult.errorDetail ?? NSNull()
            payload["provider_mode"] = "packet"
            payload["provider_last_verdict"] = "allow"
            payload["provider_last_reason"] = "provider started"
        }
        if let errorDetail = loadResult.errorDetail {
            logger.error("startFilter completed with daemon error: \(errorDetail, privacy: .public)")
        } else if loadResult.policy != nil {
            logger.info("startFilter completed with policy snapshot loaded")
        } else {
            logger.info("startFilter completed without an active policy snapshot")
        }
        completionHandler(nil)
    }

    override func stopFilter(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        let reasonDescription = String(describing: reason)
        stopHeartbeat()
        stopPacketFlushTimer()
        if #available(macOS 15.0, *) {
            handler = nil
        }
        logger.notice("stopFilter reason=\(reasonDescription, privacy: .public)")
        writeProviderStatus { payload in
            payload["provider_updated_at"] = iso8601Now()
            payload["provider_last_event"] = "stop"
            payload["provider_last_stop_reason"] = reasonDescription
        }
        completionHandler()
    }

    @discardableResult
    private func observePacket(
        interface: NWInterface,
        direction: NETrafficDirection,
        packet: UnsafeRawBufferPointer
    ) -> Bool {
        if interface.type == .loopback {
            return true
        }

        let summary = summarizePacket(
            interface: interface,
            direction: direction,
            packet: Array(packet)
        )
        guard let observation = recordPacketObservation(summary: summary) else {
            return summary.verdict != "drop"
        }

        logger.info(
            "observed packet count=\(observation.packetCount, privacy: .public) interface=\(summary.interfaceName, privacy: .public) direction=\(summary.direction, privacy: .public) length=\(summary.length, privacy: .public) src=\(summary.sourceIP ?? "<unknown>", privacy: .public) dst=\(summary.destinationIP ?? "<unknown>", privacy: .public) verdict=\(summary.verdict, privacy: .public) reason=\(summary.reason, privacy: .public)"
        )
        return summary.verdict != "drop"
    }

    private func recordPacketObservation(summary: PacketSummary) -> PendingPacketObservation? {
        packetObservationLock.lock()
        defer { packetObservationLock.unlock() }

        packetsSeen += 1
        let now = Date()
        recordTraceSampleLocked(summary: summary, observedAt: now)
        if let lastPacketStatusAt,
           now.timeIntervalSince(lastPacketStatusAt) < ProviderConstants.packetStatusThrottleSeconds
        {
            return nil
        }
        lastPacketStatusAt = now

        let observation = PendingPacketObservation(
            summary: summary,
            packetCount: packetsSeen,
            observedAt: now,
        )
        pendingPacketObservation = observation
        return observation
    }

    private func recordTraceSampleLocked(summary: PacketSummary, observedAt: Date) {
        let fingerprint = traceFingerprint(for: summary)
        if let index = traceSamples.firstIndex(where: { $0.fingerprint == fingerprint }) {
            traceSamples[index].count += 1
            traceSamples[index].lastObservedAt = observedAt
            traceDirty = true
            return
        }

        traceSamples.append(
            TracePacketSample(
                fingerprint: fingerprint,
                interfaceName: summary.interfaceName,
                direction: summary.direction,
                length: summary.length,
                etherType: summary.etherType,
                sourceIP: summary.sourceIP,
                destinationIP: summary.destinationIP,
                ipProtocol: summary.ipProtocol,
                sourcePort: summary.sourcePort,
                destinationPort: summary.destinationPort,
                verdict: summary.verdict,
                reason: summary.reason,
                guestIP: summary.guestIP,
                matchRole: summary.matchRole,
                count: 1,
                lastObservedAt: observedAt,
            )
        )
        if traceSamples.count > ProviderConstants.packetTraceLimit {
            traceSamples.removeFirst(traceSamples.count - ProviderConstants.packetTraceLimit)
        }
        traceDirty = true
    }

    private func traceFingerprint(for summary: PacketSummary) -> String {
        var components: [String] = []
        components.append(summary.interfaceName)
        components.append(summary.direction)
        components.append(summary.etherType ?? "")
        components.append(summary.sourceIP ?? "")
        components.append(summary.destinationIP ?? "")
        components.append(summary.ipProtocol.map(String.init) ?? "")
        components.append(summary.sourcePort.map(String.init) ?? "")
        components.append(summary.destinationPort.map(String.init) ?? "")
        components.append(summary.verdict)
        components.append(summary.reason)
        components.append(summary.guestIP ?? "")
        components.append(summary.matchRole)
        return components.joined(separator: "|")
    }

    private func flushPacketObservationIfNeeded() {
        let observation: PendingPacketObservation?
        let traceSamples: [TracePacketSample]
        let traceDirty: Bool
        packetObservationLock.lock()
        observation = pendingPacketObservation
        pendingPacketObservation = nil
        traceSamples = self.traceSamples
        traceDirty = self.traceDirty
        self.traceDirty = false
        packetObservationLock.unlock()

        guard observation != nil || traceDirty else {
            return
        }

        writeProviderStatus { payload in
            if let observation {
                payload["provider_updated_at"] = iso8601String(from: observation.observedAt)
                payload["provider_last_event"] = "packet"
                payload["provider_last_packet_at"] = iso8601String(from: observation.observedAt)
                payload["provider_last_verdict"] = observation.summary.verdict
                payload["provider_last_reason"] = observation.summary.reason
                payload["provider_last_error"] = NSNull()
                payload["provider_last_packet_count"] = observation.packetCount
                payload["provider_last_packet_interface"] = observation.summary.interfaceName
                payload["provider_last_packet_direction"] = observation.summary.direction
                payload["provider_last_packet_length"] = observation.summary.length
                if let etherType = observation.summary.etherType {
                    payload["provider_last_packet_ether_type"] = etherType
                }
                if let localIP = localIP(for: observation.summary) {
                    payload["provider_last_local_host"] = localIP
                }
                if let remoteIP = remoteIP(for: observation.summary) {
                    payload["provider_last_remote_host"] = remoteIP
                }
                payload["provider_last_guest_ip"] = observation.summary.guestIP ?? NSNull()
            }

            let guestRuleIPs = (currentPolicySnapshot()?.guestRules ?? []).map(\ .guestIP).sorted()
            payload["provider_trace_policy_guest_ips"] = guestRuleIPs
            payload["provider_trace_packets"] = traceSamples.map { $0.statusPayload() }
        }
    }

    private func startPacketFlushTimer() {
        packetFlushTimer?.cancel()
        let timer = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .utility))
        timer.schedule(
            deadline: .now() + ProviderConstants.packetFlushIntervalSeconds,
            repeating: ProviderConstants.packetFlushIntervalSeconds
        )
        timer.setEventHandler { [weak self] in
            _ = self?.loadPolicySnapshot()
            self?.flushPacketObservationIfNeeded()
        }
        packetFlushTimer = timer
        timer.resume()
    }

    private func stopPacketFlushTimer() {
        packetFlushTimer?.cancel()
        packetFlushTimer = nil
    }

    private func localIP(for summary: PacketSummary) -> String? {
        switch summary.direction {
        case "outbound":
            return summary.sourceIP
        case "inbound":
            return summary.destinationIP
        default:
            return summary.sourceIP
        }
    }

    private func remoteIP(for summary: PacketSummary) -> String? {
        switch summary.direction {
        case "outbound":
            return summary.destinationIP
        case "inbound":
            return summary.sourceIP
        default:
            return summary.destinationIP
        }
    }

    private func summarizePacket(
        interface: NWInterface,
        direction: NETrafficDirection,
        packet: [UInt8]
    ) -> PacketSummary {
        let interfaceName = interface.name
        let directionDescription = describe(direction: direction)
        let length = packet.count

        guard packet.count >= 14 else {
            return PacketSummary(
                interfaceName: interfaceName,
                direction: directionDescription,
                length: length,
                etherType: nil,
                sourceIP: nil,
                destinationIP: nil,
                ipProtocol: nil,
                sourcePort: nil,
                    destinationPort: nil,
                    verdict: "allow",
                    reason: "non-ethernet",
                    guestIP: nil,
                    matchRole: "none",
                )
        }

        var etherType = readUInt16(packet, offset: 12)
        var payloadOffset = 14
        if etherType == 0x8100 || etherType == 0x88A8 {
            guard packet.count >= 18 else {
                return PacketSummary(
                    interfaceName: interfaceName,
                    direction: directionDescription,
                    length: length,
                    etherType: String(format: "0x%04x", etherType),
                    sourceIP: nil,
                    destinationIP: nil,
                    ipProtocol: nil,
                    sourcePort: nil,
                    destinationPort: nil,
                    verdict: "allow",
                    reason: "truncated-vlan",
                    guestIP: nil,
                    matchRole: "none",
                )
            }
            etherType = readUInt16(packet, offset: 16)
            payloadOffset = 18
        }

        let etherTypeDescription = String(format: "0x%04x", etherType)
        let packetDetails = parsePacketDetails(packet, etherType: etherType, payloadOffset: payloadOffset)
        let decision = evaluatePacket(
            sourceIP: packetDetails.source,
            destinationIP: packetDetails.destination,
            ipProtocol: packetDetails.ipProtocol,
            sourcePort: packetDetails.sourcePort,
            destinationPort: packetDetails.destinationPort,
        )

        return PacketSummary(
            interfaceName: interfaceName,
            direction: directionDescription,
            length: length,
            etherType: etherTypeDescription,
            sourceIP: packetDetails.source,
            destinationIP: packetDetails.destination,
            ipProtocol: packetDetails.ipProtocol,
            sourcePort: packetDetails.sourcePort,
            destinationPort: packetDetails.destinationPort,
            verdict: decision.allow ? "allow" : "drop",
            reason: decision.reason,
            guestIP: decision.guestIP,
            matchRole: decision.matchRole,
        )
    }

    private func parsePacketDetails(
        _ packet: [UInt8],
        etherType: UInt16,
        payloadOffset: Int
    ) -> (source: String?, destination: String?, ipProtocol: UInt8?, sourcePort: Int?, destinationPort: Int?) {
        switch etherType {
        case 0x0800:
            guard packet.count >= payloadOffset + 20 else {
                return (nil, nil, nil, nil, nil)
            }
            let ihl = Int(packet[payloadOffset] & 0x0f) * 4
            guard ihl >= 20, packet.count >= payloadOffset + ihl else {
                return (nil, nil, nil, nil, nil)
            }
            let ipProtocol = packet[payloadOffset + 9]
            let source = ipString(packet[(payloadOffset + 12)..<(payloadOffset + 16)], family: AF_INET)
            let destination = ipString(packet[(payloadOffset + 16)..<(payloadOffset + 20)], family: AF_INET)
            let transportOffset = payloadOffset + ihl
            let ports = parseTransportPorts(packet, protocolNumber: ipProtocol, transportOffset: transportOffset)
            return (source, destination, ipProtocol, ports.source, ports.destination)
        case 0x86dd:
            guard packet.count >= payloadOffset + 40 else {
                return (nil, nil, nil, nil, nil)
            }
            let source = ipString(packet[(payloadOffset + 8)..<(payloadOffset + 24)], family: AF_INET6)
            let destination = ipString(packet[(payloadOffset + 24)..<(payloadOffset + 40)], family: AF_INET6)
            return (source, destination, packet[payloadOffset + 6], nil, nil)
        default:
            return (nil, nil, nil, nil, nil)
        }
    }

    private func parseTransportPorts(
        _ packet: [UInt8],
        protocolNumber: UInt8,
        transportOffset: Int
    ) -> (source: Int?, destination: Int?) {
        switch protocolNumber {
        case 6, 17:
            guard packet.count >= transportOffset + 4 else {
                return (nil, nil)
            }
            return (
                Int(readUInt16(packet, offset: transportOffset)),
                Int(readUInt16(packet, offset: transportOffset + 2))
            )
        default:
            return (nil, nil)
        }
    }

    private func evaluatePacket(
        sourceIP: String?,
        destinationIP: String?,
        ipProtocol: UInt8?,
        sourcePort: Int?,
        destinationPort: Int?
    ) -> (allow: Bool, reason: String, guestIP: String?, matchRole: String) {
        guard let policy = currentPolicySnapshot() else {
            return (true, "no-policy", nil, "none")
        }

        let guestRules = policy.guestRules ?? []
        guard !guestRules.isEmpty else {
            return (true, "no-guest-rules", nil, "none")
        }

        guard
            let matched = matchingGuestRule(
                guestRules: guestRules,
                sourceIP: sourceIP,
                destinationIP: destinationIP,
            )
        else {
            return (true, "untracked-guest", nil, "none")
        }

        let guestRule = matched.rule
        if matched.isIngress {
            return (true, "allow-guest-ingress", guestRule.guestIP, matched.matchRole)
        }

        if (guestRule.allowDNS ?? false), destinationPort == ProviderConstants.dnsPort, ipProtocol == 6 || ipProtocol == 17 {
            return (true, "allow-dns-bootstrap", guestRule.guestIP, matched.matchRole)
        }

        if let destinationIP, let destinationPort, isAllowedEgress(
            destinationIP: destinationIP,
            destinationPort: destinationPort,
            allowRules: guestRule.allow
        ) {
            return (true, "allow-guest-egress", guestRule.guestIP, matched.matchRole)
        }

        let defaultAction = (guestRule.defaultAction ?? policy.defaultAction).trimmingCharacters(in: .whitespacesAndNewlines)
        if stringsEqualCaseInsensitive(defaultAction, "deny") {
            return (false, "deny-guest-egress", guestRule.guestIP, matched.matchRole)
        }
        return (true, "default-allow", guestRule.guestIP, matched.matchRole)
    }

    private func currentPolicySnapshot() -> NetworkFilterDaemonPolicySnapshot? {
        cacheLock.lock()
        defer { cacheLock.unlock() }
        return cachedPolicy
    }

    private func matchingGuestRule(
        guestRules: [NetworkFilterDaemonGuestRule],
        sourceIP: String?,
        destinationIP: String?
    ) -> (rule: NetworkFilterDaemonGuestRule, isIngress: Bool, matchRole: String)? {
        for guestRule in guestRules {
            if let sourceIP, sourceIP == guestRule.guestIP {
                return (guestRule, false, "source-guest")
            }
            if let destinationIP, destinationIP == guestRule.guestIP {
                return (guestRule, true, "destination-guest")
            }
        }
        return nil
    }

    private func isAllowedEgress(
        destinationIP: String,
        destinationPort: Int,
        allowRules: [NetworkFilterDaemonAllowRule]
    ) -> Bool {
        for allowRule in allowRules {
            guard allowRule.ports.contains(destinationPort) else {
                continue
            }
            if (allowRule.remoteIPs ?? []).contains(destinationIP) {
                return true
            }
        }
        return false
    }

    private func stringsEqualCaseInsensitive(_ lhs: String, _ rhs: String) -> Bool {
        lhs.caseInsensitiveCompare(rhs) == .orderedSame
    }

    private func ipString(_ bytes: ArraySlice<UInt8>, family: Int32) -> String? {
        var copy = Array(bytes)
        var buffer = [CChar](repeating: 0, count: Int(INET6_ADDRSTRLEN))
        let result = copy.withUnsafeMutableBytes { rawBuffer in
            inet_ntop(family, rawBuffer.baseAddress, &buffer, socklen_t(buffer.count))
        }
        guard result != nil else {
            return nil
        }
        return String(cString: buffer)
    }

    private func readUInt16(_ packet: [UInt8], offset: Int) -> UInt16 {
        guard packet.count >= offset + 2 else {
            return 0
        }
        return (UInt16(packet[offset]) << 8) | UInt16(packet[offset + 1])
    }

    private func describe(direction: NETrafficDirection) -> String {
        switch direction {
        case .any:
            return "any"
        case .inbound:
            return "inbound"
        case .outbound:
            return "outbound"
        @unknown default:
            return "unknown"
        }
    }

    private func loadPolicySnapshot() -> PolicyLoadResult {
        cacheLock.lock()
        let cachedPolicy = self.cachedPolicy
        let cachedPolicyFetchedAt = self.cachedPolicyFetchedAt
        cacheLock.unlock()

        if let cachedPolicyFetchedAt,
           Date().timeIntervalSince(cachedPolicyFetchedAt) < ProviderConstants.policyCacheTTLSeconds
        {
            return PolicyLoadResult(policy: cachedPolicy, errorDetail: nil)
        }

        guard let daemonClient else {
            if let cachedPolicy {
                return PolicyLoadResult(policy: cachedPolicy, errorDetail: nil)
            }
            return PolicyLoadResult(policy: nil, errorDetail: "network filter daemon client is unavailable")
        }

        do {
            let policy = try daemonClient.getPolicy()
            cacheLock.lock()
            self.cachedPolicy = policy
            self.cachedPolicyFetchedAt = Date()
            cacheLock.unlock()
            return PolicyLoadResult(policy: policy, errorDetail: nil)
        } catch {
            if let cachedPolicy {
                return PolicyLoadResult(policy: cachedPolicy, errorDetail: nil)
            }
            return PolicyLoadResult(policy: nil, errorDetail: error.localizedDescription)
        }
    }

    private func writeProviderStatus(_ update: (inout [String: Any]) -> Void) {
        guard let daemonClient else {
            logger.error("failed to write provider status because the daemon client is unavailable")
            return
        }

        var payload: [String: Any] = ["version": 1]
        update(&payload)

        do {
            _ = try daemonClient.patchStatus(payload)
        } catch {
            logger.error("failed to write provider status to daemon: \(error.localizedDescription, privacy: .public)")
        }
    }

    private func startHeartbeat() {
        heartbeatTimer?.cancel()
        let timer = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .utility))
        timer.schedule(
            deadline: .now() + ProviderConstants.heartbeatIntervalSeconds,
            repeating: ProviderConstants.heartbeatIntervalSeconds
        )
        timer.setEventHandler { [weak self] in
            guard let self else {
                return
            }
            self.writeProviderStatus { payload in
                payload["provider_updated_at"] = self.iso8601Now()
                payload["provider_last_event"] = "heartbeat"
            }
        }
        heartbeatTimer = timer
        timer.resume()
    }

    private func stopHeartbeat() {
        heartbeatTimer?.cancel()
        heartbeatTimer = nil
    }

    private func iso8601Now() -> String {
        iso8601String(from: Date())
    }

    private func iso8601String(from date: Date) -> String {
        ISO8601DateFormatter().string(from: date)
    }
}

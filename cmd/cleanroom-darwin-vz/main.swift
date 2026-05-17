import Darwin
import Foundation
import Virtualization

private struct CLIOptions {
    let socketPath: String
}

private struct ControlRequest: Decodable {
    let op: String
    let kernelPath: String?
    let rootFSPath: String?
    let sidecarDiskPaths: [String]?
    let bootArgs: String?
    let networkMode: String?
    let vmnetSubnetCIDR: String?
    let vmnetExternalInterface: String?
    let vmnetDisableNAT44: Bool?
    let vmnetDisableNAT66: Bool?
    let vmnetDisableDNSProxy: Bool?
    let vmnetDisableRouterAdvertisement: Bool?
    let vcpus: Int?
    let memoryMiB: Int64?
    let initialMemoryBalloonTargetMiB: Int64?
    let memoryBalloonTargetMiB: Int64?
    let guestPort: UInt32?
    let launchSeconds: Int64?
    let runDir: String?
    let fileHandleSocketPath: String?
    let proxySocketPath: String?
    let consoleLogPath: String?
    let vmID: String?

    enum CodingKeys: String, CodingKey {
        case op
        case kernelPath = "kernel_path"
        case rootFSPath = "rootfs_path"
        case sidecarDiskPaths = "sidecar_disk_paths"
        case bootArgs = "boot_args"
        case networkMode = "network_mode"
        case vmnetSubnetCIDR = "vmnet_subnet_cidr"
        case vmnetExternalInterface = "vmnet_external_interface"
        case vmnetDisableNAT44 = "vmnet_disable_nat44"
        case vmnetDisableNAT66 = "vmnet_disable_nat66"
        case vmnetDisableDNSProxy = "vmnet_disable_dns_proxy"
        case vmnetDisableRouterAdvertisement = "vmnet_disable_router_advertisement"
        case vcpus
        case memoryMiB = "memory_mib"
        case initialMemoryBalloonTargetMiB = "initial_memory_balloon_target_mib"
        case memoryBalloonTargetMiB = "memory_balloon_target_mib"
        case guestPort = "guest_port"
        case launchSeconds = "launch_seconds"
        case runDir = "run_dir"
        case fileHandleSocketPath = "filehandle_socket_path"
        case proxySocketPath = "proxy_socket_path"
        case consoleLogPath = "console_log_path"
        case vmID = "vm_id"
    }
}

private struct ControlResponse: Encodable {
    let ok: Bool
    let error: String?
    let vmID: String?
    let proxySocketPath: String?
    let vmnetSubnetCIDR: String?
    let vmnetGuestIPv4: String?
    let vmnetGatewayIPv4: String?
    let vmnetPrefixLen: Int?
    let timingMS: [String: Int64]?

    enum CodingKeys: String, CodingKey {
        case ok
        case error
        case vmID = "vm_id"
        case proxySocketPath = "proxy_socket_path"
        case vmnetSubnetCIDR = "vmnet_subnet_cidr"
        case vmnetGuestIPv4 = "vmnet_guest_ipv4"
        case vmnetGatewayIPv4 = "vmnet_gateway_ipv4"
        case vmnetPrefixLen = "vmnet_prefix_len"
        case timingMS = "timing_ms"
    }
}

private enum HelperError: LocalizedError {
    case usage(String)
    case invalidRequest(String)
    case posix(String, Int32)
    case timeout(String)
    case vm(String)

    var errorDescription: String? {
        switch self {
        case .usage(let msg):
            return msg
        case .invalidRequest(let msg):
            return msg
        case .posix(let op, let code):
            return "\(op): \(String(cString: strerror(code)))"
        case .timeout(let msg):
            return msg
        case .vm(let msg):
            return msg
        }
    }
}

private final class JSONLineConnection {
    private let fd: Int32
    private var buffer = Data()

    init(fd: Int32) {
        self.fd = fd
    }

    deinit {
        _ = Darwin.close(fd)
    }

    func readRequest() throws -> ControlRequest? {
        var chunk = [UInt8](repeating: 0, count: 4096)

        while true {
            if let newline = buffer.firstIndex(of: 0x0A) {
                let line = buffer.subdata(in: 0..<newline)
                buffer.removeSubrange(0...newline)
                if line.isEmpty {
                    continue
                }
                return try JSONDecoder().decode(ControlRequest.self, from: line)
            }

            let readCount = chunk.withUnsafeMutableBytes { rawBuffer -> Int in
                guard let base = rawBuffer.baseAddress else {
                    return 0
                }
                return Darwin.read(fd, base, rawBuffer.count)
            }
            if readCount == 0 {
                if buffer.isEmpty {
                    return nil
                }
                let line = buffer
                buffer.removeAll(keepingCapacity: false)
                return try JSONDecoder().decode(ControlRequest.self, from: line)
            }
            if readCount < 0 {
                if errno == EINTR {
                    continue
                }
                throw HelperError.posix("read", errno)
            }
            buffer.append(contentsOf: chunk[0..<readCount])
        }
    }

    func writeResponse(_ res: ControlResponse) throws {
        var payload = try JSONEncoder().encode(res)
        payload.append(0x0A)
        let bytes = [UInt8](payload)
        try writeAll(dst: fd, buffer: bytes, count: bytes.count)
    }
}

private final class UnixListener {
    let path: String
    private var fd: Int32
    private let lock = NSLock()

    init(path: String) throws {
        self.path = path
        self.fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw HelperError.posix("socket", errno)
        }

        _ = path.withCString { Darwin.unlink($0) }

        var addr = sockaddr_un()
        #if os(macOS)
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        #endif
        addr.sun_family = sa_family_t(AF_UNIX)

        let pathBytes = Array(path.utf8CString)
        let maxPathBytes = MemoryLayout.size(ofValue: addr.sun_path)
        guard pathBytes.count <= maxPathBytes else {
            close()
            throw HelperError.invalidRequest("unix socket path is too long: \(path)")
        }

        withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
            ptr.withMemoryRebound(to: CChar.self, capacity: maxPathBytes) { cptr in
                cptr.initialize(repeating: 0, count: maxPathBytes)
                for i in 0..<pathBytes.count {
                    cptr[i] = pathBytes[i]
                }
            }
        }

        let addrLen = socklen_t(MemoryLayout.offset(of: \sockaddr_un.sun_path)! + pathBytes.count)
        let bindResult = withUnsafePointer(to: &addr) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { saPtr in
                Darwin.bind(fd, saPtr, addrLen)
            }
        }
        guard bindResult == 0 else {
            let code = errno
            close()
            throw HelperError.posix("bind(\(path))", code)
        }

        guard Darwin.listen(fd, 4) == 0 else {
            let code = errno
            close()
            throw HelperError.posix("listen(\(path))", code)
        }
    }

    func accept() throws -> Int32 {
        while true {
            let clientFD = Darwin.accept(fd, nil, nil)
            if clientFD >= 0 {
                return clientFD
            }
            if errno == EINTR {
                continue
            }
            throw HelperError.posix("accept(\(path))", errno)
        }
    }

    func close() {
        lock.lock()
        defer { lock.unlock() }
        if fd >= 0 {
            _ = Darwin.close(fd)
            fd = -1
        }
        _ = path.withCString { Darwin.unlink($0) }
    }

    deinit {
        close()
    }
}

private final class GuestChannel {
    let readFD: Int32
    let writeFD: Int32
    private let closeAfterUse: Bool
    private let lock = NSLock()
    private var closed = false
    private let onClose: () -> Void

    init(readFD: Int32, writeFD: Int32, closeAfterUse: Bool = true, onClose: @escaping () -> Void) {
        self.readFD = readFD
        self.writeFD = writeFD
        self.closeAfterUse = closeAfterUse
        self.onClose = onClose
    }

    func finishSession() {
        if closeAfterUse {
            close()
        }
    }

    func close() {
        lock.lock()
        if closed {
            lock.unlock()
            return
        }
        closed = true
        lock.unlock()
        onClose()
    }

    func duplicate(closeAfterUse: Bool = true) throws -> GuestChannel {
        let duplicatedReadFD = Darwin.dup(readFD)
        if duplicatedReadFD < 0 {
            throw HelperError.posix("dup(guest read fd)", errno)
        }

        let duplicatedWriteFD: Int32
        if writeFD == readFD {
            duplicatedWriteFD = duplicatedReadFD
        } else {
            duplicatedWriteFD = Darwin.dup(writeFD)
            if duplicatedWriteFD < 0 {
                let code = errno
                _ = Darwin.close(duplicatedReadFD)
                throw HelperError.posix("dup(guest write fd)", code)
            }
        }

        return GuestChannel(
            readFD: duplicatedReadFD,
            writeFD: duplicatedWriteFD,
            closeAfterUse: closeAfterUse,
            onClose: {
                if duplicatedWriteFD != duplicatedReadFD {
                    _ = Darwin.close(duplicatedWriteFD)
                }
                _ = Darwin.close(duplicatedReadFD)
            }
        )
    }
}

private final class ProxyServer {
    private let listener: UnixListener
    private let lock = NSLock()
    private var stopped = false
    private var activeChannel: GuestChannel?
    private let queue = DispatchQueue(label: "cleanroom.darwin-vz.proxy")

    init(path: String) throws {
        self.listener = try UnixListener(path: path)
    }

    func start(connectGuest: @escaping () throws -> GuestChannel) {
        queue.async { [weak self] in
            self?.serve(connectGuest: connectGuest)
        }
    }

    func stop() {
        lock.lock()
        stopped = true
        activeChannel?.close()
        activeChannel = nil
        lock.unlock()
        listener.close()
    }

    private func serve(connectGuest: @escaping () throws -> GuestChannel) {
        while true {
            if isStopped() {
                return
            }
            if !acceptAndBridge(connectGuest: connectGuest), !isStopped() {
                usleep(100_000)
            }
        }
    }

    private func acceptAndBridge(connectGuest: @escaping () throws -> GuestChannel) -> Bool {
        let hostFD: Int32
        do {
            hostFD = try listener.accept()
        } catch {
            if !isStopped() {
                fputs("cleanroom-darwin-vz proxy accept failed: \(error)\n", stderr)
            }
            return false
        }
        defer { _ = Darwin.close(hostFD) }
        if isStopped() {
            return false
        }

        let guestChannel: GuestChannel
        do {
            guestChannel = try connectGuest()
        } catch {
            fputs("cleanroom-darwin-vz guest channel connect failed: \(error)\n", stderr)
            return true
        }

        lock.lock()
        activeChannel = guestChannel
        lock.unlock()

        bridge(hostFD: hostFD, guestChannel: guestChannel)
        guestChannel.finishSession()

        lock.lock()
        if activeChannel === guestChannel {
            activeChannel = nil
        }
        lock.unlock()
        return true
    }

    private func bridge(hostFD: Int32, guestChannel: GuestChannel) {
        let guestReadFD = guestChannel.readFD
        let guestWriteFD = guestChannel.writeFD
        let group = DispatchGroup()
        let errorLock = NSLock()
        var firstError: Error?

        let captureError: (Error) -> Void = { err in
            errorLock.lock()
            defer { errorLock.unlock() }
            if firstError == nil {
                firstError = err
            }
        }

        group.enter()
        DispatchQueue.global(qos: .userInitiated).async {
            defer { group.leave() }
            defer {
                // Closing the guest side after host EOF prevents the bridge from
                // stalling on long-lived transports (for example serial fallback).
                guestChannel.close()
            }
            do {
                try pumpBytes(src: hostFD, dst: guestWriteFD)
            } catch {
                captureError(error)
            }
        }

        group.enter()
        DispatchQueue.global(qos: .userInitiated).async {
            defer { group.leave() }
            do {
                try pumpBytes(src: guestReadFD, dst: hostFD)
            } catch {
                captureError(error)
            }
            _ = Darwin.shutdown(hostFD, SHUT_WR)
        }

        group.wait()
        if let err = firstError, !isStopped() {
            fputs("cleanroom-darwin-vz proxy transport warning: \(err)\n", stderr)
        }
    }

    private func isStopped() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return stopped
    }
}

private final class VMRuntime {
    private struct NetworkDetails {
        let subnetCIDR: String
        let gatewayIPv4: String
        let guestIPv4: String
        let prefixLength: Int
    }

    private final class FileHandleNetworkAttachment {
        private let guestSocketPath: String
        private let fileHandle: FileHandle
        private let lock = NSLock()
        private var stopped = false

        init(guestSocketPath: String, fileHandle: FileHandle) {
            self.guestSocketPath = guestSocketPath
            self.fileHandle = fileHandle
        }

        func stop() {
            lock.lock()
            if stopped {
                lock.unlock()
                return
            }
            stopped = true
            lock.unlock()
            _ = guestSocketPath.withCString { Darwin.unlink($0) }
        }

        deinit {
            stop()
        }
    }

    private let lock = NSLock()
    private var vm: VZVirtualMachine?
    private var vmID: String?
    private var serialChannel: GuestChannel?
    private var guestPort: UInt32 = 0
    private var memoryMiB: Int64 = 0
    private var launchTimeout: TimeInterval = 30
    private var vmQueue: DispatchQueue?
    private var proxy: ProxyServer?
    private var fileHandleNetworkAttachment: FileHandleNetworkAttachment?

    func start(from req: ControlRequest) throws -> ControlResponse {
        guard VZVirtualMachine.isSupported else {
            throw HelperError.vm("virtualization is not supported on this host")
        }

        let kernelPath = try requireAbsolutePath(req.kernelPath, field: "kernel_path")
        let rootFSPath = try requireAbsolutePath(req.rootFSPath, field: "rootfs_path")
        let sidecarDiskPaths = try requireAbsolutePaths(req.sidecarDiskPaths ?? [], field: "sidecar_disk_paths")
        let runDir = try requireAbsolutePath(req.runDir, field: "run_dir")
        let proxySocketPath = try requireAbsolutePath(req.proxySocketPath, field: "proxy_socket_path")
        let consoleLogPath = try requireAbsolutePath(req.consoleLogPath, field: "console_log_path")

        try requireFile(kernelPath, field: "kernel_path")
        try requireFile(rootFSPath, field: "rootfs_path")
        for (index, path) in sidecarDiskPaths.enumerated() {
            try requireFile(path, field: "sidecar_disk_paths[\(index)]")
        }
        try ensureDirectory(runDir)
        try ensureDirectory((proxySocketPath as NSString).deletingLastPathComponent)
        try ensureDirectory((consoleLogPath as NSString).deletingLastPathComponent)

        let vcpus = max(1, req.vcpus ?? 1)
        let memoryMiB = max(Int64(256), req.memoryMiB ?? 512)
        let initialMemoryBalloonTargetMiB = req.initialMemoryBalloonTargetMiB ?? 0
        guard initialMemoryBalloonTargetMiB >= 0 else {
            throw HelperError.invalidRequest("initial_memory_balloon_target_mib must be non-negative")
        }
        guard initialMemoryBalloonTargetMiB <= memoryMiB else {
            throw HelperError.invalidRequest("initial_memory_balloon_target_mib cannot exceed memory_mib")
        }
        let guestPort = req.guestPort ?? 10_700
        let launchSeconds = max(Int64(5), req.launchSeconds ?? 30)
        let defaultBootArgs = "console=hvc0 root=/dev/vda rw init=/sbin/cleanroom-init cleanroom_guest_port=\(guestPort)"
        let bootArgs: String
        if let requestedBootArgs = req.bootArgs?.trimmingCharacters(in: .whitespacesAndNewlines), !requestedBootArgs.isEmpty {
            bootArgs = requestedBootArgs
        } else {
            bootArgs = defaultBootArgs
        }

        lock.lock()
        if vm != nil {
            lock.unlock()
            throw HelperError.invalidRequest("vm is already running")
        }
        lock.unlock()

        let startedAt = DispatchTime.now()
        let vmQueue = DispatchQueue(label: "cleanroom.darwin-vz.vm")
        let vmID = UUID().uuidString

        let (vm, serialChannel, networkDetails, fileHandleNetworkAttachment) = try buildVM(
            runDir: runDir,
            kernelPath: kernelPath,
            rootFSPath: rootFSPath,
            sidecarDiskPaths: sidecarDiskPaths,
            bootArgs: bootArgs,
            networkMode: req.networkMode?.trimmingCharacters(in: .whitespacesAndNewlines),
            vmnetSubnetCIDR: req.vmnetSubnetCIDR?.trimmingCharacters(in: .whitespacesAndNewlines),
            vmnetExternalInterface: req.vmnetExternalInterface?.trimmingCharacters(in: .whitespacesAndNewlines),
            vmnetDisableNAT44: req.vmnetDisableNAT44 ?? false,
            vmnetDisableNAT66: req.vmnetDisableNAT66 ?? false,
            vmnetDisableDNSProxy: req.vmnetDisableDNSProxy ?? false,
            vmnetDisableRouterAdvertisement: req.vmnetDisableRouterAdvertisement ?? false,
            vcpus: vcpus,
            memoryMiB: memoryMiB,
            fileHandleSocketPath: req.fileHandleSocketPath?.trimmingCharacters(in: .whitespacesAndNewlines),
            consoleLogPath: consoleLogPath,
            queue: vmQueue
        )
        let configBuiltAt = DispatchTime.now()
        var releaseFileHandleAttachmentOnFailure = fileHandleNetworkAttachment
        defer {
            releaseFileHandleAttachmentOnFailure?.stop()
        }

        var memoryBalloonInitialMS: Int64?
        if initialMemoryBalloonTargetMiB > 0 {
            let memoryBalloonInitialStartedAt = DispatchTime.now()
            try applyMemoryBalloonTarget(
                vm: vm,
                queue: vmQueue,
                targetMiB: initialMemoryBalloonTargetMiB,
                memoryMiB: memoryMiB
            )
            memoryBalloonInitialMS = elapsedMilliseconds(from: memoryBalloonInitialStartedAt, to: DispatchTime.now())
        }

        let vzStartStartedAt = DispatchTime.now()
        try startVM(vm, queue: vmQueue, timeoutSeconds: launchSeconds)
        let vzStartedAt = DispatchTime.now()

        let proxyReadyStartedAt = DispatchTime.now()
        let proxy = try ProxyServer(path: proxySocketPath)
        self.guestPort = guestPort
        self.launchTimeout = TimeInterval(launchSeconds)
        self.serialChannel = serialChannel
        self.vmQueue = vmQueue
        self.vm = vm
        self.vmID = vmID
        self.proxy = proxy
        self.memoryMiB = memoryMiB
        self.fileHandleNetworkAttachment = fileHandleNetworkAttachment
        releaseFileHandleAttachmentOnFailure = nil
        proxy.start { [weak self] in
            guard let self else {
                throw HelperError.vm("vm runtime no longer available")
            }
            return try self.connectGuestChannel()
        }
        let proxyReadyAt = DispatchTime.now()

        var timingMS = [
            "config_build": elapsedMilliseconds(from: startedAt, to: configBuiltAt),
            "vz_start": elapsedMilliseconds(from: vzStartStartedAt, to: vzStartedAt),
            "proxy_ready": elapsedMilliseconds(from: proxyReadyStartedAt, to: proxyReadyAt),
            "vm_ready": elapsedMilliseconds(from: startedAt, to: proxyReadyAt),
        ]
        if let memoryBalloonInitialMS {
            timingMS["memory_balloon_initial"] = memoryBalloonInitialMS
        }
        return ControlResponse(
            ok: true,
            error: nil,
            vmID: vmID,
            proxySocketPath: proxySocketPath,
            vmnetSubnetCIDR: networkDetails?.subnetCIDR,
            vmnetGuestIPv4: networkDetails?.guestIPv4,
            vmnetGatewayIPv4: networkDetails?.gatewayIPv4,
            vmnetPrefixLen: networkDetails?.prefixLength,
            timingMS: timingMS
        )
    }

    func stop(vmID requestedID: String?) throws {
        lock.lock()
        let currentID = vmID
        if let requestedID, !requestedID.isEmpty, let currentID, requestedID != currentID {
            lock.unlock()
            throw HelperError.invalidRequest("unknown vm_id \(requestedID)")
        }
        let vm = self.vm
        let serialChannel = self.serialChannel
        let vmQueue = self.vmQueue
        let proxy = self.proxy
        let fileHandleNetworkAttachment = self.fileHandleNetworkAttachment
        self.vmID = nil
        self.vm = nil
        self.serialChannel = nil
        self.guestPort = 0
        self.memoryMiB = 0
        self.launchTimeout = 30
        self.vmQueue = nil
        self.proxy = nil
        self.fileHandleNetworkAttachment = nil
        lock.unlock()

        proxy?.stop()
        serialChannel?.close()
        fileHandleNetworkAttachment?.stop()
        guard let vm else {
            return
        }
        try stopVM(vm, queue: vmQueue)
    }

    func pause(vmID requestedID: String?) throws {
        lock.lock()
        let currentID = vmID
        let vm = self.vm
        let vmQueue = self.vmQueue
        lock.unlock()

        if let requestedID, !requestedID.isEmpty, let currentID, requestedID != currentID {
            throw HelperError.invalidRequest("unknown vm_id \(requestedID)")
        }
        guard let vm else {
            throw HelperError.invalidRequest("vm is not running")
        }
        guard let vmQueue else {
            throw HelperError.vm("vm queue is unavailable")
        }
        if vm.state == .paused {
            return
        }

        let pauseSem = DispatchSemaphore(value: 0)
        var pauseError: Error?
        vmQueue.async {
            if vm.canPause {
                vm.pause { result in
                    if case .failure(let err) = result {
                        pauseError = err
                    }
                    pauseSem.signal()
                }
            } else {
                pauseError = HelperError.invalidRequest("vm cannot be paused in state \(vm.state.rawValue)")
                pauseSem.signal()
            }
        }

        if pauseSem.wait(timeout: .now() + .seconds(5)) == .timedOut {
            throw HelperError.timeout("timed out waiting for vm to pause")
        }
        if let pauseError {
            throw HelperError.vm("failed to pause vm: \(pauseError)")
        }
    }

    func resume(vmID requestedID: String?) throws {
        lock.lock()
        let currentID = vmID
        let vm = self.vm
        let vmQueue = self.vmQueue
        lock.unlock()

        if let requestedID, !requestedID.isEmpty, let currentID, requestedID != currentID {
            throw HelperError.invalidRequest("unknown vm_id \(requestedID)")
        }
        guard let vm else {
            throw HelperError.invalidRequest("vm is not running")
        }
        guard let vmQueue else {
            throw HelperError.vm("vm queue is unavailable")
        }
        if vm.state == .running {
            return
        }

        let resumeSem = DispatchSemaphore(value: 0)
        var resumeError: Error?
        vmQueue.async {
            if vm.canResume {
                vm.resume { result in
                    if case .failure(let err) = result {
                        resumeError = err
                    }
                    resumeSem.signal()
                }
            } else {
                resumeError = HelperError.invalidRequest("vm cannot be resumed in state \(vm.state.rawValue)")
                resumeSem.signal()
            }
        }

        if resumeSem.wait(timeout: .now() + .seconds(5)) == .timedOut {
            throw HelperError.timeout("timed out waiting for vm to resume")
        }
        if let resumeError {
            throw HelperError.vm("failed to resume vm: \(resumeError)")
        }
    }

    func setMemoryBalloonTarget(vmID requestedID: String?, targetMiB requestedTargetMiB: Int64?) throws {
        guard let targetMiB = requestedTargetMiB else {
            throw HelperError.invalidRequest("missing memory_balloon_target_mib")
        }

        lock.lock()
        let currentID = vmID
        let vm = self.vm
        let vmQueue = self.vmQueue
        let memoryMiB = self.memoryMiB
        lock.unlock()

        if let requestedID, !requestedID.isEmpty, let currentID, requestedID != currentID {
            throw HelperError.invalidRequest("unknown vm_id \(requestedID)")
        }
        guard let vm else {
            throw HelperError.invalidRequest("vm is not running")
        }
        guard let vmQueue else {
            throw HelperError.vm("vm queue is unavailable")
        }
        guard memoryMiB > 0 else {
            throw HelperError.vm("vm memory size is unavailable")
        }

        try applyMemoryBalloonTarget(
            vm: vm,
            queue: vmQueue,
            targetMiB: targetMiB,
            memoryMiB: memoryMiB
        )
    }

    private func parseIPv4Address(_ value: String) throws -> in_addr {
        var address = in_addr()
        let result = value.withCString { cs in
            inet_pton(AF_INET, cs, &address)
        }
        guard result == 1 else {
            throw HelperError.invalidRequest("invalid IPv4 address \(value)")
        }
        return address
    }

    private func ipv4Mask(prefixLength: Int) -> in_addr {
        in_addr(s_addr: ipv4MaskValue(prefixLength: prefixLength).bigEndian)
    }

    private func ipv4MaskValue(prefixLength: Int) -> UInt32 {
        if prefixLength <= 0 {
            return 0
        }
        return UInt32.max << UInt32(32 - prefixLength)
    }

    private func formatIPv4Address(_ value: UInt32) throws -> String {
        var address = in_addr(s_addr: value.bigEndian)
        var buffer = [CChar](repeating: 0, count: Int(INET_ADDRSTRLEN))
        let result = withUnsafePointer(to: &address) { addrPointer in
            inet_ntop(AF_INET, addrPointer, &buffer, socklen_t(INET_ADDRSTRLEN))
        }
        guard result != nil else {
            throw HelperError.vm("format IPv4 address \(value)")
        }
        return String(cString: buffer)
    }

    private func staticNetworkDetails(subnetCIDR: String?, defaultSubnetCIDR: String) throws -> NetworkDetails {
        let resolvedSubnetCIDR: String
        if let subnetCIDR, !subnetCIDR.isEmpty {
            resolvedSubnetCIDR = subnetCIDR
        } else {
            resolvedSubnetCIDR = defaultSubnetCIDR
        }

        let components = resolvedSubnetCIDR.split(separator: "/", omittingEmptySubsequences: false)
        guard components.count == 2, let prefixLength = Int(components[1]), (0...32).contains(prefixLength) else {
            throw HelperError.invalidRequest("vmnet_subnet_cidr must be a valid IPv4 CIDR")
        }

        let subnetAddress = try parseIPv4Address(String(components[0]))
        let maskValue = ipv4MaskValue(prefixLength: prefixLength)
        let networkValue = UInt32(bigEndian: subnetAddress.s_addr) & maskValue
        let upperValue = networkValue + ~maskValue
        let gatewayValue = networkValue + 1
        let guestValue = networkValue + 2
        guard guestValue < upperValue else {
            throw HelperError.invalidRequest("network subnet \(resolvedSubnetCIDR) must provide at least four addresses")
        }

        return NetworkDetails(
            subnetCIDR: "\(try formatIPv4Address(networkValue))/\(prefixLength)",
            gatewayIPv4: try formatIPv4Address(gatewayValue),
            guestIPv4: try formatIPv4Address(guestValue),
            prefixLength: prefixLength
        )
    }

    private func createFileHandleNetworkAttachment(
        runDir: String,
        socketPath: String,
        networkDevice: VZVirtioNetworkDeviceConfiguration,
        details _: NetworkDetails
    ) throws -> FileHandleNetworkAttachment {
        let guestSocketPath = (runDir as NSString).appendingPathComponent("g.sock")
        let guestFD = try createConnectedUnixDatagramSocket(localPath: guestSocketPath, remotePath: socketPath)
        let attachmentHandle = FileHandle(fileDescriptor: guestFD, closeOnDealloc: true)

        do {
            try writeAll(dst: guestFD, buffer: Array("VFKT".utf8), count: 4)
            let attachment = VZFileHandleNetworkDeviceAttachment(fileHandle: attachmentHandle)
            attachment.maximumTransmissionUnit = 1500
            networkDevice.attachment = attachment
            return FileHandleNetworkAttachment(guestSocketPath: guestSocketPath, fileHandle: attachmentHandle)
        } catch {
            _ = guestSocketPath.withCString { Darwin.unlink($0) }
            throw error
        }
    }

    private func createConnectedUnixDatagramSocket(localPath: String, remotePath: String) throws -> Int32 {
        guard !localPath.isEmpty else {
            throw HelperError.invalidRequest("file-handle guest socket path is empty")
        }
        guard !remotePath.isEmpty else {
            throw HelperError.invalidRequest("file-handle gateway socket path is empty")
        }

        let fd = Darwin.socket(AF_UNIX, SOCK_DGRAM, 0)
        guard fd >= 0 else {
            throw HelperError.posix("socket(filehandle network)", errno)
        }

        var closeFD = true
        var releaseLocalPath = true
        defer {
            if releaseLocalPath {
                _ = localPath.withCString { Darwin.unlink($0) }
            }
        }
        defer {
            if closeFD, fd >= 0 {
                _ = Darwin.close(fd)
            }
        }

        _ = localPath.withCString { Darwin.unlink($0) }

        try setSocketBuffer(fd: fd, option: SO_SNDBUF, value: 1 * 1024 * 1024, label: "setsockopt(SO_SNDBUF)")
        try setSocketBuffer(fd: fd, option: SO_RCVBUF, value: 4 * 1024 * 1024, label: "setsockopt(SO_RCVBUF)")

        var localAddress = try unixSocketAddress(path: localPath)
        let bindResult = withUnsafePointer(to: &localAddress.addr) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { saPtr in
                Darwin.bind(fd, saPtr, localAddress.len)
            }
        }
        guard bindResult == 0 else {
            throw HelperError.posix("bind(\(localPath))", errno)
        }

        var remoteAddress = try unixSocketAddress(path: remotePath)
        let connectResult = withUnsafePointer(to: &remoteAddress.addr) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { saPtr in
                Darwin.connect(fd, saPtr, remoteAddress.len)
            }
        }
        guard connectResult == 0 else {
            throw HelperError.posix("connect(\(remotePath))", errno)
        }

        releaseLocalPath = false
        closeFD = false
        return fd
    }

    private func setSocketBuffer(fd: Int32, option: Int32, value: Int32, label: String) throws {
        var bufferSize = value
        let result = withUnsafePointer(to: &bufferSize) { ptr in
            Darwin.setsockopt(fd, SOL_SOCKET, option, ptr, socklen_t(MemoryLayout<Int32>.size))
        }
        guard result == 0 else {
            throw HelperError.posix(label, errno)
        }
    }

    private func unixSocketAddress(path: String) throws -> (addr: sockaddr_un, len: socklen_t) {
        var addr = sockaddr_un()
        #if os(macOS)
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        #endif
        addr.sun_family = sa_family_t(AF_UNIX)

        let pathBytes = Array(path.utf8CString)
        let maxPathBytes = MemoryLayout.size(ofValue: addr.sun_path)
        guard pathBytes.count <= maxPathBytes else {
            throw HelperError.invalidRequest("unix socket path is too long: \(path)")
        }

        withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
            ptr.withMemoryRebound(to: CChar.self, capacity: maxPathBytes) { cptr in
                cptr.initialize(repeating: 0, count: maxPathBytes)
                for i in 0..<pathBytes.count {
                    cptr[i] = pathBytes[i]
                }
            }
        }

        let addrLen = socklen_t(MemoryLayout.offset(of: \sockaddr_un.sun_path)! + pathBytes.count)
        return (addr, addrLen)
    }

    private func buildVM(
        runDir: String,
        kernelPath: String,
        rootFSPath: String,
        sidecarDiskPaths: [String],
        bootArgs: String,
        networkMode: String?,
        vmnetSubnetCIDR: String?,
        vmnetExternalInterface: String?,
        vmnetDisableNAT44: Bool,
        vmnetDisableNAT66: Bool,
        vmnetDisableDNSProxy: Bool,
        vmnetDisableRouterAdvertisement: Bool,
        vcpus: Int,
        memoryMiB: Int64,
        fileHandleSocketPath: String?,
        consoleLogPath: String,
        queue: DispatchQueue
    ) throws -> (VZVirtualMachine, GuestChannel, NetworkDetails?, FileHandleNetworkAttachment?) {
        let kernelURL = URL(fileURLWithPath: kernelPath)
        let rootFSURL = URL(fileURLWithPath: rootFSPath)
        let consoleURL = URL(fileURLWithPath: consoleLogPath)

        let networkDevice = VZVirtioNetworkDeviceConfiguration()
        let resolvedNetworkMode = (networkMode?.isEmpty == false ? networkMode! : "filehandle")
        let networkDetails: NetworkDetails?
        let fileHandleNetworkAttachment: FileHandleNetworkAttachment?
        var resolvedBootArgs = bootArgs
        if (vmnetExternalInterface?.isEmpty == false)
            || vmnetDisableNAT44
            || vmnetDisableNAT66
            || vmnetDisableDNSProxy
            || vmnetDisableRouterAdvertisement {
            throw HelperError.invalidRequest("darwin-vz no longer supports vmnet-specific network settings")
        }
        switch resolvedNetworkMode {
        case "filehandle":
            guard let fileHandleSocketPath, !fileHandleSocketPath.isEmpty else {
                throw HelperError.invalidRequest("missing filehandle_socket_path for filehandle network mode")
            }
            let details = try staticNetworkDetails(subnetCIDR: vmnetSubnetCIDR, defaultSubnetCIDR: "10.233.0.0/24")
            let attachment = try createFileHandleNetworkAttachment(
                runDir: runDir,
                socketPath: fileHandleSocketPath,
                networkDevice: networkDevice,
                details: details
            )
            resolvedBootArgs += " cleanroom_vmnet_guest_ipv4=\(details.guestIPv4)"
            resolvedBootArgs += " cleanroom_vmnet_gateway_ipv4=\(details.gatewayIPv4)"
            resolvedBootArgs += " cleanroom_vmnet_prefix_len=\(details.prefixLength)"
            resolvedBootArgs += " cleanroom_vmnet_subnet_cidr=\(details.subnetCIDR)"
            networkDetails = details
            fileHandleNetworkAttachment = attachment
        default:
            throw HelperError.invalidRequest("unsupported network_mode \(resolvedNetworkMode); only filehandle is supported")
        }

        let bootLoader = VZLinuxBootLoader(kernelURL: kernelURL)
        bootLoader.commandLine = resolvedBootArgs

        let config = VZVirtualMachineConfiguration()
        config.bootLoader = bootLoader
        config.cpuCount = vcpus
        config.memorySize = UInt64(memoryMiB) * 1024 * 1024

        let serialAttachment = try VZFileSerialPortAttachment(url: consoleURL, append: false)
        let serial = VZVirtioConsoleDeviceSerialPortConfiguration()
        serial.attachment = serialAttachment

        let hostToGuest = Pipe()
        let guestToHost = Pipe()
        let execAttachment = VZFileHandleSerialPortAttachment(
            fileHandleForReading: hostToGuest.fileHandleForReading,
            fileHandleForWriting: guestToHost.fileHandleForWriting
        )
        let execPort = VZVirtioConsoleDeviceSerialPortConfiguration()
        execPort.attachment = execAttachment
        config.serialPorts = [serial, execPort]

        let diskAttachment = try VZDiskImageStorageDeviceAttachment(url: rootFSURL, readOnly: false)
        let blockDevice = VZVirtioBlockDeviceConfiguration(attachment: diskAttachment)
        var storageDevices: [VZStorageDeviceConfiguration] = [blockDevice]
        for sidecarPath in sidecarDiskPaths {
            let sidecarURL = URL(fileURLWithPath: sidecarPath)
            let sidecarAttachment = try VZDiskImageStorageDeviceAttachment(url: sidecarURL, readOnly: false)
            storageDevices.append(VZVirtioBlockDeviceConfiguration(attachment: sidecarAttachment))
        }
        config.storageDevices = storageDevices
        config.networkDevices = [networkDevice]

        config.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        config.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
        config.socketDevices = [VZVirtioSocketDeviceConfiguration()]

        do {
            try config.validate()
        } catch {
            fileHandleNetworkAttachment?.stop()
            throw error
        }
        let channel = GuestChannel(
            readFD: guestToHost.fileHandleForReading.fileDescriptor,
            writeFD: hostToGuest.fileHandleForWriting.fileDescriptor,
            closeAfterUse: false,
            onClose: {
                guestToHost.fileHandleForReading.closeFile()
                hostToGuest.fileHandleForWriting.closeFile()
            }
        )
        return (VZVirtualMachine(configuration: config, queue: queue), channel, networkDetails, fileHandleNetworkAttachment)
    }

    private func applyMemoryBalloonTarget(
        vm: VZVirtualMachine,
        queue: DispatchQueue,
        targetMiB: Int64,
        memoryMiB: Int64,
        timeoutSeconds: Int64 = 5
    ) throws {
        guard targetMiB > 0 else {
            throw HelperError.invalidRequest("memory_balloon_target_mib must be greater than zero")
        }
        guard targetMiB <= memoryMiB else {
            throw HelperError.invalidRequest("memory_balloon_target_mib cannot exceed memory_mib")
        }
        let bytesPerMiB = Int64(1024 * 1024)
        guard targetMiB <= Int64.max / bytesPerMiB else {
            throw HelperError.invalidRequest("memory_balloon_target_mib is too large")
        }
        let targetBytes = UInt64(targetMiB * bytesPerMiB)

        let sem = DispatchSemaphore(value: 0)
        var targetError: Error?
        queue.async {
            guard let balloon = vm.memoryBalloonDevices.first as? VZVirtioTraditionalMemoryBalloonDevice else {
                targetError = HelperError.vm("vm memory balloon device is unavailable")
                sem.signal()
                return
            }
            balloon.targetVirtualMachineMemorySize = targetBytes
            sem.signal()
        }

        if sem.wait(timeout: .now() + .seconds(Int(timeoutSeconds))) == .timedOut {
            throw HelperError.timeout("timed out waiting for memory balloon target update")
        }
        if let targetError {
            throw targetError
        }
    }

    private func startVM(_ vm: VZVirtualMachine, queue: DispatchQueue, timeoutSeconds: Int64) throws {
        let sem = DispatchSemaphore(value: 0)
        var startError: Error?

        queue.async {
            vm.start { result in
                if case .failure(let err) = result {
                    startError = err
                }
                sem.signal()
            }
        }

        if sem.wait(timeout: .now() + .seconds(Int(timeoutSeconds))) == .timedOut {
            throw HelperError.timeout("timed out waiting for vm to start")
        }
        if let startError {
            throw HelperError.vm("failed to start vm: \(startError)")
        }
    }

    private func stopVM(_ vm: VZVirtualMachine, queue: DispatchQueue?) throws {
        let workQueue = queue ?? DispatchQueue(label: "cleanroom.darwin-vz.vm.stop")

        let requestStopSem = DispatchSemaphore(value: 0)
        workQueue.async {
            if vm.canRequestStop {
                _ = try? vm.requestStop()
            }
            requestStopSem.signal()
        }
        _ = requestStopSem.wait(timeout: .now() + .seconds(2))

        if #available(macOS 12.0, *) {
            let stopSem = DispatchSemaphore(value: 0)
            var stopError: Error?
            workQueue.async {
                if vm.canStop {
                    vm.stop { err in
                        stopError = err
                        stopSem.signal()
                    }
                } else {
                    stopSem.signal()
                }
            }
            _ = stopSem.wait(timeout: .now() + .seconds(5))
            if let stopError {
                throw HelperError.vm("failed to stop vm: \(stopError)")
            }
        }
    }

    private func connectGuestChannel() throws -> GuestChannel {
        lock.lock()
        let vm = self.vm
        let vmQueue = self.vmQueue
        let guestPort = self.guestPort
        let timeout = self.launchTimeout
        let serialChannel = self.serialChannel
        lock.unlock()

        guard let vm else {
            throw HelperError.vm("vm is not running")
        }
        guard vm.state == .running else {
            throw HelperError.vm("vm is not running")
        }
        guard let vmQueue else {
            throw HelperError.vm("vm queue is unavailable")
        }

        if let socketDevice = vm.socketDevices.first as? VZVirtioSocketDevice {
            let deadline = Date().addingTimeInterval(timeout)
            var lastError: Error?
            while Date() < deadline {
                let sem = DispatchSemaphore(value: 0)
                var resultConnection: VZVirtioSocketConnection?
                var resultError: Error?

                vmQueue.async {
                    socketDevice.connect(toPort: guestPort) { result in
                        switch result {
                        case .success(let conn):
                            resultConnection = conn
                        case .failure(let err):
                            resultError = err
                        }
                        sem.signal()
                    }
                }

                let remaining = max(0, deadline.timeIntervalSinceNow)
                if sem.wait(timeout: .now() + remaining) == .timedOut {
                    break
                }
                if let resultConnection {
                    return GuestChannel(
                        readFD: resultConnection.fileDescriptor,
                        writeFD: resultConnection.fileDescriptor,
                        onClose: { resultConnection.close() }
                    )
                }
                if let resultError {
                    lastError = resultError
                } else {
                    lastError = HelperError.vm("guest vsock connect returned no connection")
                }
                usleep(10_000)
            }
            if let lastError {
                fputs("cleanroom-darwin-vz: vsock connect fallback to serial after error: \(lastError)\n", stderr)
            } else {
                fputs("cleanroom-darwin-vz: vsock connect timed out, falling back to serial\n", stderr)
            }
        }

        guard let serialChannel else {
            throw HelperError.vm("guest serial channel is not available")
        }
        return try serialChannel.duplicate()
    }
}

private final class HelperService {
    private let socketPath: String
    private let vmRuntime = VMRuntime()

    init(socketPath: String) {
        self.socketPath = socketPath
    }

    func run() throws {
        let listener = try UnixListener(path: socketPath)
        defer {
            try? vmRuntime.stop(vmID: nil)
            listener.close()
        }

        let controlFD = try listener.accept()
        let conn = JSONLineConnection(fd: controlFD)

        while true {
            guard let req = try readRequest(conn) else {
                break
            }

            do {
                let response = try handle(req)
                try conn.writeResponse(response)
            } catch {
                try conn.writeResponse(ControlResponse(
                    ok: false,
                    error: error.localizedDescription,
                    vmID: nil,
                    proxySocketPath: nil,
                    vmnetSubnetCIDR: nil,
                    vmnetGuestIPv4: nil,
                    vmnetGatewayIPv4: nil,
                    vmnetPrefixLen: nil,
                    timingMS: nil
                ))
            }
        }
    }

    private func readRequest(_ conn: JSONLineConnection) throws -> ControlRequest? {
        do {
            return try conn.readRequest()
        } catch {
            throw HelperError.invalidRequest("failed to decode control request: \(error)")
        }
    }

    private func handle(_ req: ControlRequest) throws -> ControlResponse {
        switch req.op {
        case "StartVM":
            return try vmRuntime.start(from: req)
        case "StopVM":
            try vmRuntime.stop(vmID: req.vmID)
            return ControlResponse(ok: true, error: nil, vmID: nil, proxySocketPath: nil, vmnetSubnetCIDR: nil, vmnetGuestIPv4: nil, vmnetGatewayIPv4: nil, vmnetPrefixLen: nil, timingMS: nil)
        case "PauseVM":
            try vmRuntime.pause(vmID: req.vmID)
            return ControlResponse(ok: true, error: nil, vmID: nil, proxySocketPath: nil, vmnetSubnetCIDR: nil, vmnetGuestIPv4: nil, vmnetGatewayIPv4: nil, vmnetPrefixLen: nil, timingMS: nil)
        case "ResumeVM":
            try vmRuntime.resume(vmID: req.vmID)
            return ControlResponse(ok: true, error: nil, vmID: nil, proxySocketPath: nil, vmnetSubnetCIDR: nil, vmnetGuestIPv4: nil, vmnetGatewayIPv4: nil, vmnetPrefixLen: nil, timingMS: nil)
        case "SetMemoryBalloonTarget":
            try vmRuntime.setMemoryBalloonTarget(vmID: req.vmID, targetMiB: req.memoryBalloonTargetMiB)
            return ControlResponse(ok: true, error: nil, vmID: nil, proxySocketPath: nil, vmnetSubnetCIDR: nil, vmnetGuestIPv4: nil, vmnetGatewayIPv4: nil, vmnetPrefixLen: nil, timingMS: nil)
        case "Ping":
            return ControlResponse(ok: true, error: nil, vmID: nil, proxySocketPath: nil, vmnetSubnetCIDR: nil, vmnetGuestIPv4: nil, vmnetGatewayIPv4: nil, vmnetPrefixLen: nil, timingMS: nil)
        default:
            throw HelperError.invalidRequest("unsupported op \(req.op)")
        }
    }
}

private func parseCLI() throws -> CLIOptions {
    var socketPath = ""

    var i = 1
    while i < CommandLine.arguments.count {
        let arg = CommandLine.arguments[i]
        switch arg {
        case "--socket":
            i += 1
            guard i < CommandLine.arguments.count else {
                throw HelperError.usage("missing value for --socket")
            }
            socketPath = CommandLine.arguments[i]
        case "--help", "-h":
            throw HelperError.usage("usage: cleanroom-darwin-vz --socket /abs/path/helper.sock")
        default:
            throw HelperError.usage("unknown argument \(arg)")
        }
        i += 1
    }

    if socketPath.isEmpty {
        throw HelperError.usage("missing --socket")
    }
    if !socketPath.hasPrefix("/") {
        throw HelperError.usage("--socket path must be absolute")
    }
    return CLIOptions(socketPath: socketPath)
}

private func requireAbsolutePath(_ rawValue: String?, field: String) throws -> String {
    guard let rawValue else {
        throw HelperError.invalidRequest("missing \(field)")
    }
    let path = rawValue.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !path.isEmpty else {
        throw HelperError.invalidRequest("missing \(field)")
    }
    guard path.hasPrefix("/") else {
        throw HelperError.invalidRequest("\(field) must be absolute")
    }
    return path
}

private func requireAbsolutePaths(_ rawValues: [String], field: String) throws -> [String] {
    try rawValues.enumerated().map { index, value in
        try requireAbsolutePath(value, field: "\(field)[\(index)]")
    }
}

private func requireFile(_ path: String, field: String) throws {
    var isDir: ObjCBool = false
    guard FileManager.default.fileExists(atPath: path, isDirectory: &isDir), !isDir.boolValue else {
        throw HelperError.invalidRequest("\(field) does not exist: \(path)")
    }
}

private func ensureDirectory(_ path: String) throws {
    guard !path.isEmpty else {
        throw HelperError.invalidRequest("directory path is empty")
    }
    try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true)
}

private func elapsedMilliseconds(from start: DispatchTime, to end: DispatchTime) -> Int64 {
    guard end.uptimeNanoseconds >= start.uptimeNanoseconds else {
        return 0
    }
    return Int64((end.uptimeNanoseconds - start.uptimeNanoseconds) / 1_000_000)
}

private func pumpBytes(src: Int32, dst: Int32) throws {
    var buffer = [UInt8](repeating: 0, count: 64 * 1024)

    while true {
        let readCount = buffer.withUnsafeMutableBytes { rawBuffer -> Int in
            guard let base = rawBuffer.baseAddress else {
                return 0
            }
            return Darwin.read(src, base, rawBuffer.count)
        }

        if readCount == 0 {
            return
        }
        if readCount < 0 {
            if errno == EINTR {
                continue
            }
            throw HelperError.posix("read", errno)
        }

        try writeAll(dst: dst, buffer: buffer, count: readCount)
    }
}

private func writeAll(dst: Int32, buffer: [UInt8], count: Int) throws {
    var offset = 0
    while offset < count {
        let written = buffer.withUnsafeBytes { rawBuffer -> Int in
            guard let base = rawBuffer.baseAddress else {
                return 0
            }
            return Darwin.write(dst, base.advanced(by: offset), count - offset)
        }
        if written < 0 {
            if errno == EINTR {
                continue
            }
            throw HelperError.posix("write", errno)
        }
        if written == 0 {
            throw HelperError.vm("short write on proxy stream")
        }
        offset += written
    }
}

do {
    let options = try parseCLI()
    let service = HelperService(socketPath: options.socketPath)
    try service.run()
} catch {
    fputs("cleanroom-darwin-vz: \(error.localizedDescription)\n", stderr)
    exit(1)
}

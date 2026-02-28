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
    let bootArgs: String?
    let vcpus: Int?
    let memoryMiB: Int64?
    let guestPort: UInt32?
    let launchSeconds: Int64?
    let runDir: String?
    let proxySocketPath: String?
    let consoleLogPath: String?
    let vmID: String?

    enum CodingKeys: String, CodingKey {
        case op
        case kernelPath = "kernel_path"
        case rootFSPath = "rootfs_path"
        case bootArgs = "boot_args"
        case vcpus
        case memoryMiB = "memory_mib"
        case guestPort = "guest_port"
        case launchSeconds = "launch_seconds"
        case runDir = "run_dir"
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
    let timingMS: [String: Int64]?

    enum CodingKeys: String, CodingKey {
        case ok
        case error
        case vmID = "vm_id"
        case proxySocketPath = "proxy_socket_path"
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
    private var closed = false
    private let onClose: () -> Void

    init(readFD: Int32, writeFD: Int32, onClose: @escaping () -> Void) {
        self.readFD = readFD
        self.writeFD = writeFD
        self.onClose = onClose
    }

    func close() {
        if closed {
            return
        }
        closed = true
        onClose()
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
            self?.acceptAndBridge(connectGuest: connectGuest)
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

    private func acceptAndBridge(connectGuest: @escaping () throws -> GuestChannel) {
        let hostFD: Int32
        do {
            hostFD = try listener.accept()
        } catch {
            if !isStopped() {
                fputs("cleanroom-darwin-vz proxy accept failed: \(error)\n", stderr)
            }
            return
        }

        defer { _ = Darwin.close(hostFD) }
        if isStopped() {
            return
        }

        let guestChannel: GuestChannel
        do {
            guestChannel = try connectGuest()
        } catch {
            fputs("cleanroom-darwin-vz guest channel connect failed: \(error)\n", stderr)
            return
        }

        lock.lock()
        activeChannel = guestChannel
        lock.unlock()

        bridge(hostFD: hostFD, guestReadFD: guestChannel.readFD, guestWriteFD: guestChannel.writeFD)
        guestChannel.close()

        lock.lock()
        if activeChannel === guestChannel {
            activeChannel = nil
        }
        lock.unlock()
    }

    private func bridge(hostFD: Int32, guestReadFD: Int32, guestWriteFD: Int32) {
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
    private let lock = NSLock()
    private var vm: VZVirtualMachine?
    private var vmID: String?
    private var serialChannel: GuestChannel?
    private var guestPort: UInt32 = 0
    private var launchTimeout: TimeInterval = 30
    private var vmQueue: DispatchQueue?
    private var proxy: ProxyServer?

    func start(from req: ControlRequest) throws -> ControlResponse {
        guard VZVirtualMachine.isSupported else {
            throw HelperError.vm("virtualization is not supported on this host")
        }

        let kernelPath = try requireAbsolutePath(req.kernelPath, field: "kernel_path")
        let rootFSPath = try requireAbsolutePath(req.rootFSPath, field: "rootfs_path")
        let runDir = try requireAbsolutePath(req.runDir, field: "run_dir")
        let proxySocketPath = try requireAbsolutePath(req.proxySocketPath, field: "proxy_socket_path")
        let consoleLogPath = try requireAbsolutePath(req.consoleLogPath, field: "console_log_path")

        try requireFile(kernelPath, field: "kernel_path")
        try requireFile(rootFSPath, field: "rootfs_path")
        try ensureDirectory(runDir)
        try ensureDirectory((proxySocketPath as NSString).deletingLastPathComponent)
        try ensureDirectory((consoleLogPath as NSString).deletingLastPathComponent)

        let vcpus = max(1, req.vcpus ?? 1)
        let memoryMiB = max(Int64(256), req.memoryMiB ?? 512)
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

        let vmQueue = DispatchQueue(label: "cleanroom.darwin-vz.vm")
        let vmID = UUID().uuidString
        let startedAt = Date()

        let (vm, serialChannel) = try buildVM(
            kernelPath: kernelPath,
            rootFSPath: rootFSPath,
            bootArgs: bootArgs,
            vcpus: vcpus,
            memoryMiB: memoryMiB,
            consoleLogPath: consoleLogPath,
            queue: vmQueue
        )

        try startVM(vm, queue: vmQueue, timeoutSeconds: launchSeconds)

        let proxy = try ProxyServer(path: proxySocketPath)
        self.guestPort = guestPort
        self.launchTimeout = TimeInterval(launchSeconds)
        self.serialChannel = serialChannel
        self.vmQueue = vmQueue
        self.vm = vm
        self.vmID = vmID
        self.proxy = proxy
        proxy.start { [weak self] in
            guard let self else {
                throw HelperError.vm("vm runtime no longer available")
            }
            return try self.connectGuestChannel()
        }

        let vmReadyMS = Int64(Date().timeIntervalSince(startedAt) * 1000)
        return ControlResponse(
            ok: true,
            error: nil,
            vmID: vmID,
            proxySocketPath: proxySocketPath,
            timingMS: ["vm_ready": vmReadyMS]
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
        self.vmID = nil
        self.vm = nil
        self.serialChannel = nil
        self.guestPort = 0
        self.launchTimeout = 30
        self.vmQueue = nil
        self.proxy = nil
        lock.unlock()

        proxy?.stop()
        serialChannel?.close()
        guard let vm else {
            return
        }
        try stopVM(vm, queue: vmQueue)
    }

    private func buildVM(
        kernelPath: String,
        rootFSPath: String,
        bootArgs: String,
        vcpus: Int,
        memoryMiB: Int64,
        consoleLogPath: String,
        queue: DispatchQueue
    ) throws -> (VZVirtualMachine, GuestChannel) {
        let kernelURL = URL(fileURLWithPath: kernelPath)
        let rootFSURL = URL(fileURLWithPath: rootFSPath)
        let consoleURL = URL(fileURLWithPath: consoleLogPath)

        let bootLoader = VZLinuxBootLoader(kernelURL: kernelURL)
        bootLoader.commandLine = bootArgs

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
        config.storageDevices = [blockDevice]

        let networkDevice = VZVirtioNetworkDeviceConfiguration()
        networkDevice.attachment = VZNATNetworkDeviceAttachment()
        config.networkDevices = [networkDevice]

        config.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        config.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
        config.socketDevices = [VZVirtioSocketDeviceConfiguration()]

        try config.validate()
        let channel = GuestChannel(
            readFD: guestToHost.fileHandleForReading.fileDescriptor,
            writeFD: hostToGuest.fileHandleForWriting.fileDescriptor,
            onClose: {
                guestToHost.fileHandleForReading.closeFile()
                hostToGuest.fileHandleForWriting.closeFile()
            }
        )
        return (VZVirtualMachine(configuration: config, queue: queue), channel)
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
                usleep(100_000)
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
        return serialChannel
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
            return ControlResponse(ok: true, error: nil, vmID: nil, proxySocketPath: nil, timingMS: nil)
        case "Ping":
            return ControlResponse(ok: true, error: nil, vmID: nil, proxySocketPath: nil, timingMS: nil)
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

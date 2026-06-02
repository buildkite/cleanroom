import Darwin
import Foundation
import Virtualization

private struct Options {
    var bundlePath = ""
    var command: [String] = ["/usr/bin/sw_vers"]
    var metricsPath = ""
    var validateOnly = false
    var timeoutSeconds = 120.0
    var connectIntervalMS: useconds_t = 50
    var agentName = "root"
}

private struct BundleManifest: Decodable {
    let schemaVersion: Int
    let os: String
    let arch: String
    let macOSVersion: String?
    let macOSBuild: String?
    let vcpus: Int
    let memoryMiB: UInt64
    let disk: String
    let auxiliaryStorage: String
    let hardwareModel: String
    let machineIdentifier: String
    let agent: AgentManifest
    let userAgent: AgentManifest?
    let display: DisplayManifest?

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case os
        case arch
        case macOSVersion = "macos_version"
        case macOSBuild = "macos_build"
        case vcpus
        case memoryMiB = "memory_mib"
        case disk
        case auxiliaryStorage = "auxiliary_storage"
        case hardwareModel = "hardware_model"
        case machineIdentifier = "machine_identifier"
        case agent
        case userAgent = "user_agent"
        case display
    }
}

private struct AgentManifest: Decodable {
    let transport: String
    let port: UInt32
    let version: String
}

private struct DisplayManifest: Decodable {
    let widthPx: Int?
    let heightPx: Int?
    let pixelsPerInch: Int?

    enum CodingKeys: String, CodingKey {
        case widthPx = "width_px"
        case heightPx = "height_px"
        case pixelsPerInch = "pixels_per_inch"
    }
}

private struct ResolvedBundle {
    let manifestURL: URL
    let manifest: BundleManifest
    let diskURL: URL
    let auxiliaryStorageURL: URL
    let hardwareModel: VZMacHardwareModel
    let machineIdentifier: VZMacMachineIdentifier
}

private func validateAgent(_ agent: AgentManifest, field: String) throws {
    guard agent.transport == "virtio_socket" else {
        throw RunnerError.invalid("\(field).transport must be virtio_socket")
    }
    guard agent.port > 0 else {
        throw RunnerError.invalid("\(field).port must be greater than zero")
    }
    guard !agent.version.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
        throw RunnerError.invalid("\(field).version must not be empty")
    }
}

private func selectedAgent(bundle: ResolvedBundle, name: String) throws -> AgentManifest {
    switch name {
    case "root":
        return bundle.manifest.agent
    case "user":
        guard let userAgent = bundle.manifest.userAgent else {
            throw RunnerError.invalid("bundle does not declare user_agent")
        }
        return userAgent
    default:
        throw RunnerError.invalid("--agent must be root or user")
    }
}

private struct ExecRequest: Encodable {
    let command: [String]
    let env: [String]
    let dir: String?

    enum CodingKeys: String, CodingKey {
        case command
        case env
        case dir
    }
}

private struct ExecInputFrame: Encodable {
    let type: String
}

private struct ExecFrame: Decodable {
    let type: String
    let data: Data?
    let exitCode: Int?
    let error: String?

    enum CodingKeys: String, CodingKey {
        case type
        case data
        case exitCode = "exit_code"
        case error
    }
}

private struct ProbeResult: Encodable {
    let bundle: String
    let command: [String]
    let startedVM: Bool
    let startMS: Double?
    let vsockConnectMS: Double?
    let execResponseMS: Double?
    let exitCode: Int?
    let error: String?
    let selectedAgent: String
    let macOSVersion: String?
    let macOSBuild: String?
    let agentVersion: String
    let vcpus: Int
    let memoryMiB: UInt64

    enum CodingKeys: String, CodingKey {
        case bundle
        case command
        case startedVM = "started_vm"
        case startMS = "start_ms"
        case vsockConnectMS = "vsock_connect_ms"
        case execResponseMS = "exec_response_ms"
        case exitCode = "exit_code"
        case error
        case selectedAgent = "selected_agent"
        case macOSVersion = "macos_version"
        case macOSBuild = "macos_build"
        case agentVersion = "agent_version"
        case vcpus
        case memoryMiB = "memory_mib"
    }
}

private enum RunnerError: LocalizedError {
    case usage(String)
    case invalid(String)
    case timeout(String)
    case posix(String, Int32)
    case vm(String)

    var errorDescription: String? {
        switch self {
        case .usage(let message), .invalid(let message), .timeout(let message), .vm(let message):
            return message
        case .posix(let op, let code):
            return "\(op): \(String(cString: strerror(code)))"
        }
    }
}

private final class VMHandle {
    let vm: VZVirtualMachine
    let queue: DispatchQueue

    init(vm: VZVirtualMachine, queue: DispatchQueue) {
        self.vm = vm
        self.queue = queue
    }

    func stop() {
        let requestStopSem = DispatchSemaphore(value: 0)
        queue.async {
            if self.vm.canRequestStop {
                _ = try? self.vm.requestStop()
            }
            requestStopSem.signal()
        }
        _ = requestStopSem.wait(timeout: .now() + .seconds(3))

        let stopSem = DispatchSemaphore(value: 0)
        queue.async {
            if self.vm.canStop {
                self.vm.stop { _ in stopSem.signal() }
            } else {
                stopSem.signal()
            }
        }
        _ = stopSem.wait(timeout: .now() + .seconds(10))
    }
}

private func usage() -> String {
    """
    Usage:
      darwin-vz-macos-minimal --bundle <bundle.json|bundle-dir> [options] [-- <command> [args...]]

    Options:
      --metrics <path>       Write result JSON to path. If omitted, JSON is written to stderr.
      --agent <root|user>    Agent endpoint to use. Default: root.
      --validate-only        Validate the bundle and host support without starting the VM.
      --timeout <seconds>    VM start, connect, and command timeout. Default: 120.
      -h, --help             Show this help.

    The default command is /usr/bin/sw_vers.
    """
}

private func parseOptions(_ args: [String]) throws -> Options {
    var opts = Options()
    var i = 0
    while i < args.count {
        let arg = args[i]
        if arg == "--" {
            opts.command = Array(args.dropFirst(i + 1))
            break
        }

        func value() throws -> String {
            guard i + 1 < args.count else {
                throw RunnerError.usage("missing value for \(arg)\n\n\(usage())")
            }
            i += 1
            return args[i]
        }

        switch arg {
        case "--bundle":
            opts.bundlePath = try value()
        case "--metrics":
            opts.metricsPath = try value()
            if opts.metricsPath == "-" {
                throw RunnerError.usage("--metrics - is not supported because guest stdout is streamed on stdout; write metrics to a file or omit --metrics")
            }
        case "--agent":
            opts.agentName = try value()
            guard opts.agentName == "root" || opts.agentName == "user" else {
                throw RunnerError.invalid("--agent must be root or user")
            }
        case "--validate-only":
            opts.validateOnly = true
        case "--timeout":
            guard let timeout = Double(try value()), timeout > 0 else {
                throw RunnerError.invalid("invalid --timeout")
            }
            opts.timeoutSeconds = timeout
        case "-h", "--help":
            throw RunnerError.usage(usage())
        default:
            throw RunnerError.usage("unknown argument: \(arg)\n\n\(usage())")
        }
        i += 1
    }

    if opts.bundlePath.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
        throw RunnerError.usage("missing --bundle\n\n\(usage())")
    }
    if opts.command.isEmpty {
        throw RunnerError.invalid("command after -- must not be empty")
    }
    if opts.command.contains(where: { $0.isEmpty }) {
        throw RunnerError.invalid("command arguments must not be empty")
    }
    return opts
}

private func now() -> UInt64 {
    DispatchTime.now().uptimeNanoseconds
}

private func msSince(_ start: UInt64) -> Double {
    Double(now() - start) / 1_000_000.0
}

private func absoluteURL(_ path: String, relativeTo baseURL: URL) -> URL {
    let expanded = NSString(string: path).expandingTildeInPath
    if expanded.hasPrefix("/") {
        return URL(fileURLWithPath: expanded)
    }
    return baseURL.appendingPathComponent(expanded)
}

private func manifestURL(from path: String) throws -> URL {
    let expanded = NSString(string: path).expandingTildeInPath
    var isDir: ObjCBool = false
    guard FileManager.default.fileExists(atPath: expanded, isDirectory: &isDir) else {
        throw RunnerError.invalid("bundle path does not exist: \(expanded)")
    }
    if isDir.boolValue {
        return URL(fileURLWithPath: expanded).appendingPathComponent("bundle.json")
    }
    return URL(fileURLWithPath: expanded)
}

private func requireReadableFile(_ url: URL, field: String) throws {
    var isDir: ObjCBool = false
    guard FileManager.default.fileExists(atPath: url.path, isDirectory: &isDir), !isDir.boolValue else {
        throw RunnerError.invalid("\(field) does not exist or is not a file: \(url.path)")
    }
}

private func loadBundle(path: String) throws -> ResolvedBundle {
    let url = try manifestURL(from: path)
    try requireReadableFile(url, field: "bundle manifest")
    let baseURL = url.deletingLastPathComponent()
    let manifest = try JSONDecoder().decode(BundleManifest.self, from: Data(contentsOf: url))

    guard manifest.schemaVersion == 1 else {
        throw RunnerError.invalid("unsupported schema_version \(manifest.schemaVersion)")
    }
    guard manifest.os == "macos" else {
        throw RunnerError.invalid("bundle os must be macos")
    }
    guard manifest.arch == "arm64" else {
        throw RunnerError.invalid("bundle arch must be arm64")
    }
    guard manifest.vcpus > 0 else {
        throw RunnerError.invalid("vcpus must be greater than zero")
    }
    guard manifest.memoryMiB >= 1024 else {
        throw RunnerError.invalid("memory_mib must be at least 1024")
    }
    guard manifest.memoryMiB <= UInt64.max / 1024 / 1024 else {
        throw RunnerError.invalid("memory_mib is too large")
    }
    try validateAgent(manifest.agent, field: "agent")
    if let userAgent = manifest.userAgent {
        try validateAgent(userAgent, field: "user_agent")
        if userAgent.port == manifest.agent.port {
            throw RunnerError.invalid("user_agent.port must differ from agent.port")
        }
    }
    if let display = manifest.display {
        if let width = display.widthPx, width <= 0 {
            throw RunnerError.invalid("display.width_px must be greater than zero")
        }
        if let height = display.heightPx, height <= 0 {
            throw RunnerError.invalid("display.height_px must be greater than zero")
        }
        if let pixelsPerInch = display.pixelsPerInch, pixelsPerInch <= 0 {
            throw RunnerError.invalid("display.pixels_per_inch must be greater than zero")
        }
    }

    let diskURL = absoluteURL(manifest.disk, relativeTo: baseURL)
    let auxiliaryURL = absoluteURL(manifest.auxiliaryStorage, relativeTo: baseURL)
    let hardwareModelURL = absoluteURL(manifest.hardwareModel, relativeTo: baseURL)
    let machineIdentifierURL = absoluteURL(manifest.machineIdentifier, relativeTo: baseURL)
    try requireReadableFile(diskURL, field: "disk")
    try requireReadableFile(auxiliaryURL, field: "auxiliary_storage")
    try requireReadableFile(hardwareModelURL, field: "hardware_model")
    try requireReadableFile(machineIdentifierURL, field: "machine_identifier")

    guard let hardwareModel = VZMacHardwareModel(dataRepresentation: try Data(contentsOf: hardwareModelURL)) else {
        throw RunnerError.invalid("hardware_model is not a valid VZMacHardwareModel data representation")
    }
    guard hardwareModel.isSupported else {
        throw RunnerError.invalid("hardware_model is not supported by this host")
    }
    guard let machineIdentifier = VZMacMachineIdentifier(dataRepresentation: try Data(contentsOf: machineIdentifierURL)) else {
        throw RunnerError.invalid("machine_identifier is not a valid VZMacMachineIdentifier data representation")
    }

    return ResolvedBundle(
        manifestURL: url,
        manifest: manifest,
        diskURL: diskURL,
        auxiliaryStorageURL: auxiliaryURL,
        hardwareModel: hardwareModel,
        machineIdentifier: machineIdentifier
    )
}

private func writeAll(fd: Int32, bytes: [UInt8]) throws {
    var written = 0
    while written < bytes.count {
        let n = bytes.withUnsafeBytes { raw -> Int in
            guard let base = raw.baseAddress else { return 0 }
            return Darwin.write(fd, base.advanced(by: written), bytes.count - written)
        }
        if n < 0 {
            if errno == EINTR {
                continue
            }
            throw RunnerError.posix("write", errno)
        }
        if n == 0 {
            throw RunnerError.posix("write", EIO)
        }
        written += n
    }
}

private func waitForReadable(fd: Int32, deadline: Date) throws {
    while true {
        let remaining = deadline.timeIntervalSinceNow
        if remaining <= 0 {
            throw RunnerError.timeout("timed out waiting for guest frame")
        }
        let timeoutMS = Int32(max(1, min(remaining * 1000, Double(Int32.max))))
        var pfd = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
        let n = Darwin.poll(&pfd, 1, timeoutMS)
        if n < 0 {
            if errno == EINTR {
                continue
            }
            throw RunnerError.posix("poll", errno)
        }
        if n == 0 {
            throw RunnerError.timeout("timed out waiting for guest frame")
        }
        if (pfd.revents & Int16(POLLIN | POLLHUP | POLLERR | POLLNVAL)) != 0 {
            return
        }
    }
}

private func readLine(fd: Int32, buffer: inout Data, deadline: Date) throws -> Data {
    var chunk = [UInt8](repeating: 0, count: 4096)
    while true {
        if let newline = buffer.firstIndex(of: 0x0A) {
            let line = buffer.subdata(in: 0..<newline)
            buffer.removeSubrange(0...newline)
            return line
        }
        try waitForReadable(fd: fd, deadline: deadline)
        let chunkCount = chunk.count
        let n = chunk.withUnsafeMutableBytes { raw -> Int in
            guard let base = raw.baseAddress else { return 0 }
            return Darwin.read(fd, base, chunkCount)
        }
        if n < 0 {
            if errno == EINTR {
                continue
            }
            throw RunnerError.posix("read", errno)
        }
        if n == 0 {
            throw RunnerError.vm("guest connection closed before exit frame")
        }
        buffer.append(contentsOf: chunk[0..<n])
    }
}

private func buildVM(bundle: ResolvedBundle, queue: DispatchQueue) throws -> VMHandle {
    guard VZVirtualMachine.isSupported else {
        throw RunnerError.vm("Virtualization.framework is not supported on this host")
    }

    let config = VZVirtualMachineConfiguration()
    config.bootLoader = VZMacOSBootLoader()
    config.cpuCount = bundle.manifest.vcpus
    config.memorySize = bundle.manifest.memoryMiB * 1024 * 1024

    let platform = VZMacPlatformConfiguration()
    platform.hardwareModel = bundle.hardwareModel
    platform.machineIdentifier = bundle.machineIdentifier
    platform.auxiliaryStorage = VZMacAuxiliaryStorage(url: bundle.auxiliaryStorageURL)
    config.platform = platform

    let display = bundle.manifest.display
    let graphics = VZMacGraphicsDeviceConfiguration()
    graphics.displays = [
        VZMacGraphicsDisplayConfiguration(
            widthInPixels: display?.widthPx ?? 1024,
            heightInPixels: display?.heightPx ?? 768,
            pixelsPerInch: display?.pixelsPerInch ?? 72
        )
    ]
    config.graphicsDevices = [graphics]
    config.keyboards = [VZUSBKeyboardConfiguration()]
    config.pointingDevices = [VZUSBScreenCoordinatePointingDeviceConfiguration()]

    let disk = try VZDiskImageStorageDeviceAttachment(
        url: bundle.diskURL,
        readOnly: false,
        cachingMode: .automatic,
        synchronizationMode: .full
    )
    config.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: disk)]
    config.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    config.socketDevices = [VZVirtioSocketDeviceConfiguration()]

    try config.validate()
    return VMHandle(vm: VZVirtualMachine(configuration: config, queue: queue), queue: queue)
}

private func startVM(_ handle: VMHandle, timeoutSeconds: Double) throws {
    let sem = DispatchSemaphore(value: 0)
    var startError: Error?
    handle.queue.async {
        handle.vm.start { result in
            if case .failure(let error) = result {
                startError = error
            }
            sem.signal()
        }
    }
    if sem.wait(timeout: .now() + timeoutSeconds) == .timedOut {
        throw RunnerError.timeout("timed out waiting for VM start")
    }
    if let startError {
        throw RunnerError.vm("VM start failed: \(startError)")
    }
}

private func connectVsock(handle: VMHandle, port: UInt32, timeoutSeconds: Double, intervalMS: useconds_t) throws -> VZVirtioSocketConnection {
    guard let socketDevice = handle.vm.socketDevices.first as? VZVirtioSocketDevice else {
        throw RunnerError.vm("VM has no virtio socket device")
    }
    let deadline = Date().addingTimeInterval(timeoutSeconds)
    var lastError: Error?
    while Date() < deadline {
        let sem = DispatchSemaphore(value: 0)
        var connection: VZVirtioSocketConnection?
        var connectError: Error?
        handle.queue.async {
            socketDevice.connect(toPort: port) { result in
                switch result {
                case .success(let conn):
                    connection = conn
                case .failure(let error):
                    connectError = error
                }
                sem.signal()
            }
        }
        _ = sem.wait(timeout: .now() + .milliseconds(500))
        if let connection {
            return connection
        }
        if let connectError {
            lastError = connectError
        }
        usleep(intervalMS * 1000)
    }
    if let lastError {
        throw RunnerError.timeout("timed out connecting to guest vsock port \(port): \(lastError)")
    }
    throw RunnerError.timeout("timed out connecting to guest vsock port \(port)")
}

private func writeExecRequest(connection: VZVirtioSocketConnection, command: [String]) throws {
    let request = ExecRequest(command: command, env: [], dir: nil)
    let encoded = try JSONEncoder().encode(request)
    try writeAll(fd: connection.fileDescriptor, bytes: Array(encoded) + [0x0A])
    let eof = try JSONEncoder().encode(ExecInputFrame(type: "eof"))
    try writeAll(fd: connection.fileDescriptor, bytes: Array(eof) + [0x0A])
}

private func runGuestCommand(connection: VZVirtioSocketConnection, command: [String], timeoutSeconds: Double) throws -> (Int, String?) {
    try writeExecRequest(connection: connection, command: command)
    let deadline = Date().addingTimeInterval(timeoutSeconds)
    var buffer = Data()
    while true {
        let line = try readLine(fd: connection.fileDescriptor, buffer: &buffer, deadline: deadline)
        if line.isEmpty {
            continue
        }
        let frame = try JSONDecoder().decode(ExecFrame.self, from: line)
        switch frame.type {
        case "stdout":
            if let data = frame.data {
                try writeAll(fd: STDOUT_FILENO, bytes: Array(data))
            }
        case "stderr":
            if let data = frame.data {
                try writeAll(fd: STDERR_FILENO, bytes: Array(data))
            }
        case "exit":
            let exitCode = frame.exitCode ?? 0
            if exitCode < 0 {
                return (1, frame.error ?? "guest command exited without a status")
            }
            guard (0...255).contains(exitCode) else {
                throw RunnerError.vm("guest exit_code must be between 0 and 255")
            }
            return (exitCode, frame.error)
        default:
            throw RunnerError.vm("unknown guest frame type \(frame.type)")
        }
    }
}

private func writeMetrics(_ result: ProbeResult, path: String) throws {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.sortedKeys]
    let encoded = try encoder.encode(result)
    if path == "-" {
        print(String(data: encoded, encoding: .utf8)!)
        return
    }
    if path.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
        fputs(String(data: encoded, encoding: .utf8)! + "\n", stderr)
        return
    }
    try encoded.write(to: URL(fileURLWithPath: NSString(string: path).expandingTildeInPath))
}

do {
    let opts = try parseOptions(Array(CommandLine.arguments.dropFirst()))
    let bundle = try loadBundle(path: opts.bundlePath)
    let agent = try selectedAgent(bundle: bundle, name: opts.agentName)
    if opts.validateOnly {
        let queue = DispatchQueue(label: "cleanroom.benchmark.darwin-vz-macos-minimal.validate")
        _ = try buildVM(bundle: bundle, queue: queue)
        let result = ProbeResult(
            bundle: bundle.manifestURL.path,
            command: opts.command,
            startedVM: false,
            startMS: nil,
            vsockConnectMS: nil,
            execResponseMS: nil,
            exitCode: nil,
            error: nil,
            selectedAgent: opts.agentName,
            macOSVersion: bundle.manifest.macOSVersion,
            macOSBuild: bundle.manifest.macOSBuild,
            agentVersion: agent.version,
            vcpus: bundle.manifest.vcpus,
            memoryMiB: bundle.manifest.memoryMiB
        )
        try writeMetrics(result, path: opts.metricsPath)
        Foundation.exit(0)
    }

    let queue = DispatchQueue(label: "cleanroom.benchmark.darwin-vz-macos-minimal.vm")
    let t0 = now()
    let handle = try buildVM(bundle: bundle, queue: queue)
    defer { handle.stop() }

    try startVM(handle, timeoutSeconds: opts.timeoutSeconds)
    let startMS = msSince(t0)
    let connection = try connectVsock(
        handle: handle,
        port: agent.port,
        timeoutSeconds: opts.timeoutSeconds,
        intervalMS: opts.connectIntervalMS
    )
    defer { connection.close() }
    let connectMS = msSince(t0)
    let outcome = try runGuestCommand(connection: connection, command: opts.command, timeoutSeconds: opts.timeoutSeconds)
    let execResponseMS = msSince(t0)
    let result = ProbeResult(
        bundle: bundle.manifestURL.path,
        command: opts.command,
        startedVM: true,
        startMS: startMS,
        vsockConnectMS: connectMS,
        execResponseMS: execResponseMS,
        exitCode: outcome.0,
        error: outcome.1,
        selectedAgent: opts.agentName,
        macOSVersion: bundle.manifest.macOSVersion,
        macOSBuild: bundle.manifest.macOSBuild,
        agentVersion: agent.version,
        vcpus: bundle.manifest.vcpus,
        memoryMiB: bundle.manifest.memoryMiB
    )
    try writeMetrics(result, path: opts.metricsPath)
    Foundation.exit(Int32(outcome.0))
} catch RunnerError.usage(let message) {
    fputs(message + "\n", stderr)
    Foundation.exit(message.hasPrefix("Usage:") ? 0 : 2)
} catch {
    fputs("darwin-vz-macos-minimal: \(error.localizedDescription)\n", stderr)
    Foundation.exit(1)
}

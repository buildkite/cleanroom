import Darwin
import Foundation
import Virtualization

private let maxPreProbeBalloonSettleMS = UInt64(Int32.max)

private struct Options {
    var kernelPath = ""
    var rootFSPath = ""
    var initrdPath = ""
    var bootArgs = ""
    var consoleLogPath = ""
    var guestPort: UInt32 = 10700
    var vcpus = 2
    var memoryMiB: UInt64 = 1024
    var probe = "exec"
    var probeMemoryMiB: UInt64 = 256
    var probePreTouchMS: UInt64 = 500
    var probeHoldMS: UInt64 = 1000
    var probePostFreeMS: UInt64 = 3000
    var balloonDevice = true
    var initialBalloonTargetMiB: UInt64?
    var preProbeBalloonTargetMiB: UInt64?
    var preProbeBalloonSettleMS: UInt64 = 1000
    var balloonTargetMiB: UInt64?
    var timeoutSeconds = 30.0
    var connectIntervalMS: useconds_t = 10
}

private struct ProbeResult: Encodable {
    let probe: String
    let startMS: Double
    let vsockConnectMS: Double
    let execResponseMS: Double
    let probeDurationMS: Double
    let exitCode: Int
    let error: String?
    let guestTimingMS: [String: Int64]?
    let guestMemoryEvents: [GuestMemoryEvent]?
    let hostMemorySamples: [HostMemorySample]?
    let vcpus: Int
    let memoryMiB: UInt64
    let balloonDevice: Bool
    let initialBalloonTargetMiB: UInt64?
    let preProbeBalloonTargetMiB: UInt64?
    let preProbeBalloonSettleMS: UInt64
    let balloonTargetMiB: UInt64?

    enum CodingKeys: String, CodingKey {
        case probe
        case startMS = "start_ms"
        case vsockConnectMS = "vsock_connect_ms"
        case execResponseMS = "exec_response_ms"
        case probeDurationMS = "probe_duration_ms"
        case exitCode = "exit_code"
        case error
        case guestTimingMS = "guest_timing_ms"
        case guestMemoryEvents = "guest_memory_events"
        case hostMemorySamples = "host_memory_samples"
        case vcpus
        case memoryMiB = "memory_mib"
        case balloonDevice = "balloon_device"
        case initialBalloonTargetMiB = "initial_balloon_target_mib"
        case preProbeBalloonTargetMiB = "pre_probe_balloon_target_mib"
        case preProbeBalloonSettleMS = "pre_probe_balloon_settle_ms"
        case balloonTargetMiB = "balloon_target_mib"
    }
}

private struct ExecRequest: Encodable {
    let command: [String]
    let closedEnv: Bool

    enum CodingKeys: String, CodingKey {
        case command
        case closedEnv = "closed_env"
    }
}

private struct ExecFrame: Decodable {
    let type: String
    let data: Data?
    let exitCode: Int?
    let error: String?
    let guestTimingMS: [String: Int64]?

    enum CodingKeys: String, CodingKey {
        case type
        case data
        case exitCode = "exit_code"
        case error
        case guestTimingMS = "guest_timing_ms"
    }
}

private struct HostMemorySample: Encodable {
    let label: String
    let elapsedMS: Double
    let runnerResidentSizeBytes: UInt64
    let runnerPhysFootprintBytes: UInt64
    let virtualizationResidentSizeBytes: UInt64
    let virtualizationPhysFootprintBytes: UInt64
    let virtualizationPIDs: [Int32]

    enum CodingKeys: String, CodingKey {
        case label
        case elapsedMS = "elapsed_ms"
        case runnerResidentSizeBytes = "runner_resident_size_bytes"
        case runnerPhysFootprintBytes = "runner_phys_footprint_bytes"
        case virtualizationResidentSizeBytes = "virtualization_resident_size_bytes"
        case virtualizationPhysFootprintBytes = "virtualization_phys_footprint_bytes"
        case virtualizationPIDs = "virtualization_pids"
    }
}

private struct GuestMemoryEvent: Codable {
    let phase: String
    let elapsedMS: Int64
    let allocatedMiB: UInt64?
    let meminfo: [String: UInt64]?
    let error: String?

    enum CodingKeys: String, CodingKey {
        case phase
        case elapsedMS = "elapsed_ms"
        case allocatedMiB = "allocated_mib"
        case meminfo
        case error
    }
}

private struct ProbeOutcome {
    let exitCode: Int
    let error: String?
    let guestTimingMS: [String: Int64]?
    let guestMemoryEvents: [GuestMemoryEvent]
    let hostMemorySamples: [HostMemorySample]
}

private enum BaselineError: LocalizedError {
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
    let excludedVirtualizationPIDs: Set<pid_t>
    var virtualizationPIDs: [pid_t] = []

    init(vm: VZVirtualMachine, queue: DispatchQueue, excludedVirtualizationPIDs: Set<pid_t>) {
        self.vm = vm
        self.queue = queue
        self.excludedVirtualizationPIDs = excludedVirtualizationPIDs
    }

    func stop() {
        let requestStopSem = DispatchSemaphore(value: 0)
        queue.async {
            if self.vm.canRequestStop {
                _ = try? self.vm.requestStop()
            }
            requestStopSem.signal()
        }
        _ = requestStopSem.wait(timeout: .now() + .seconds(1))

        let stopSem = DispatchSemaphore(value: 0)
        queue.async {
            if self.vm.canStop {
                self.vm.stop { _ in stopSem.signal() }
            } else {
                stopSem.signal()
            }
        }
        _ = stopSem.wait(timeout: .now() + .seconds(3))
    }
}

private func usage() -> String {
    """
    Usage:
      darwin-vz-minimal --kernel <vmlinux> (--rootfs <rootfs.ext4> | --initrd <initramfs.cpio.gz>) [options]

    Options:
      --boot-args <args>          Full Linux command line. If omitted, a default line is selected for the boot mode.
      --initrd <path>             Linux initramfs image. If no rootfs is provided, no block storage is attached.
      --console-log <path>        Console log path. Default: temporary file under /tmp.
      --guest-port <port>         Guest vsock port. Default: 10700.
      --vcpus <count>            vCPU count. Default: 2.
      --memory-mib <mib>         Guest memory. Default: 1024.
      --probe <name>             Probe to run: exec or memory-reporting. Default: exec.
      --probe-memory-mib <mib>   Memory touched by memory-reporting probe. Default: 256.
      --probe-pre-touch-ms <ms>  Delay after the before sample. Default: 500.
      --probe-hold-ms <ms>       Delay after touching memory. Default: 1000.
      --probe-post-free-ms <ms>  Delay after freeing memory. Default: 3000.
      --balloon-device <on|off>  Attach the VZ virtio memory balloon. Default: on.
      --initial-balloon-target-mib <mib>
                                  Set VZ balloon target before VM start.
      --pre-probe-balloon-target-mib <mib>
                                  Set VZ balloon target before running the probe.
      --pre-probe-balloon-settle-ms <ms>
                                  Delay after pre-probe balloon target. Default: 1000.
      --balloon-target-mib <mib> Set explicit VZ balloon target after the guest frees memory.
      --timeout <seconds>        Start/probe timeout. Default: 30.

    The rootfs mode must already contain Cleanroom's guest runtime. Use a writable throwaway copy.
    The initrd mode expects an /init process that accepts Cleanroom's guest exec JSON protocol on vsock.
    The exec probe runs /bin/true. The memory-reporting probe runs /bin/memprobe and samples host footprint while guest memory is touched and freed.
    """
}

private func parseOptions(_ args: [String]) throws -> Options {
    var opts = Options()
    var i = 0
    while i < args.count {
        let arg = args[i]
        func value() throws -> String {
            guard i + 1 < args.count else {
                throw BaselineError.usage("missing value for \(arg)\n\n\(usage())")
            }
            i += 1
            return args[i]
        }

        switch arg {
        case "--kernel":
            opts.kernelPath = try value()
        case "--rootfs":
            opts.rootFSPath = try value()
        case "--initrd":
            opts.initrdPath = try value()
        case "--boot-args":
            opts.bootArgs = try value()
        case "--console-log":
            opts.consoleLogPath = try value()
        case "--guest-port":
            guard let port = UInt32(try value()) else {
                throw BaselineError.invalid("invalid --guest-port")
            }
            opts.guestPort = port
        case "--vcpus":
            guard let vcpus = Int(try value()), vcpus > 0 else {
                throw BaselineError.invalid("invalid --vcpus")
            }
            opts.vcpus = vcpus
        case "--memory-mib":
            guard let memory = UInt64(try value()), memory > 0 else {
                throw BaselineError.invalid("invalid --memory-mib")
            }
            opts.memoryMiB = memory
        case "--probe":
            opts.probe = try value()
            guard ["exec", "memory-reporting"].contains(opts.probe) else {
                throw BaselineError.invalid("invalid --probe")
            }
        case "--probe-memory-mib":
            guard let memory = UInt64(try value()), memory > 0 else {
                throw BaselineError.invalid("invalid --probe-memory-mib")
            }
            opts.probeMemoryMiB = memory
        case "--probe-pre-touch-ms":
            guard let preTouch = UInt64(try value()) else {
                throw BaselineError.invalid("invalid --probe-pre-touch-ms")
            }
            opts.probePreTouchMS = preTouch
        case "--probe-hold-ms":
            guard let hold = UInt64(try value()) else {
                throw BaselineError.invalid("invalid --probe-hold-ms")
            }
            opts.probeHoldMS = hold
        case "--probe-post-free-ms":
            guard let postFree = UInt64(try value()) else {
                throw BaselineError.invalid("invalid --probe-post-free-ms")
            }
            opts.probePostFreeMS = postFree
        case "--balloon-device":
            switch try value() {
            case "on":
                opts.balloonDevice = true
            case "off":
                opts.balloonDevice = false
            default:
                throw BaselineError.invalid("invalid --balloon-device")
            }
        case "--initial-balloon-target-mib":
            guard let memory = UInt64(try value()), memory > 0 else {
                throw BaselineError.invalid("invalid --initial-balloon-target-mib")
            }
            opts.initialBalloonTargetMiB = memory
        case "--pre-probe-balloon-target-mib":
            guard let memory = UInt64(try value()), memory > 0 else {
                throw BaselineError.invalid("invalid --pre-probe-balloon-target-mib")
            }
            opts.preProbeBalloonTargetMiB = memory
        case "--pre-probe-balloon-settle-ms":
            guard let settle = UInt64(try value()) else {
                throw BaselineError.invalid("invalid --pre-probe-balloon-settle-ms")
            }
            opts.preProbeBalloonSettleMS = settle
        case "--balloon-target-mib":
            guard let memory = UInt64(try value()), memory > 0 else {
                throw BaselineError.invalid("invalid --balloon-target-mib")
            }
            opts.balloonTargetMiB = memory
        case "--timeout":
            guard let timeout = Double(try value()), timeout > 0 else {
                throw BaselineError.invalid("invalid --timeout")
            }
            opts.timeoutSeconds = timeout
        case "-h", "--help":
            throw BaselineError.usage(usage())
        default:
            throw BaselineError.usage("unknown argument: \(arg)\n\n\(usage())")
        }
        i += 1
    }

    if opts.kernelPath.isEmpty || (opts.rootFSPath.isEmpty && opts.initrdPath.isEmpty) {
        throw BaselineError.usage("missing --kernel and either --rootfs or --initrd\n\n\(usage())")
    }
    if opts.probe == "memory-reporting" && !opts.rootFSPath.isEmpty {
        throw BaselineError.invalid("memory-reporting probe currently requires initrd mode")
    }
    if opts.probe == "memory-reporting" && opts.probeMemoryMiB >= opts.memoryMiB {
        throw BaselineError.invalid("--probe-memory-mib must be smaller than --memory-mib")
    }
    if let balloonTargetMiB = opts.balloonTargetMiB, balloonTargetMiB > opts.memoryMiB {
        throw BaselineError.invalid("--balloon-target-mib must be less than or equal to --memory-mib")
    }
    if let targetMiB = opts.initialBalloonTargetMiB, targetMiB > opts.memoryMiB {
        throw BaselineError.invalid("--initial-balloon-target-mib must be less than or equal to --memory-mib")
    }
    if let targetMiB = opts.preProbeBalloonTargetMiB, targetMiB > opts.memoryMiB {
        throw BaselineError.invalid("--pre-probe-balloon-target-mib must be less than or equal to --memory-mib")
    }
    if !opts.balloonDevice && (opts.initialBalloonTargetMiB != nil || opts.preProbeBalloonTargetMiB != nil || opts.balloonTargetMiB != nil) {
        throw BaselineError.invalid("balloon target options require --balloon-device on")
    }
    if opts.balloonTargetMiB != nil && opts.probe != "memory-reporting" {
        throw BaselineError.invalid("--balloon-target-mib requires --probe memory-reporting")
    }
    if opts.preProbeBalloonSettleMS > maxPreProbeBalloonSettleMS {
        throw BaselineError.invalid("--pre-probe-balloon-settle-ms must be less than or equal to \(maxPreProbeBalloonSettleMS)")
    }
    if opts.consoleLogPath.isEmpty {
        opts.consoleLogPath = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("darwin-vz-minimal-\(UUID().uuidString).console.log")
            .path
    }
    if opts.bootArgs.isEmpty {
        if opts.rootFSPath.isEmpty {
            opts.bootArgs = "console=hvc0 rdinit=/init cleanroom_guest_port=\(opts.guestPort) cleanroom_guest_boot_timing=1"
        } else {
            opts.bootArgs = "console=hvc0 root=/dev/vda rw init=/usr/local/bin/cleanroom-guest-agent cleanroom_guest_port=\(opts.guestPort) cleanroom_guest_boot_timing=1"
        }
    }
    return opts
}

private func now() -> UInt64 {
    DispatchTime.now().uptimeNanoseconds
}

private func msSince(_ start: UInt64) -> Double {
    Double(now() - start) / 1_000_000.0
}

private func sleepMilliseconds(_ milliseconds: UInt64) throws {
    if milliseconds == 0 {
        return
    }
    var request = timespec(
        tv_sec: time_t(milliseconds / 1000),
        tv_nsec: CLong((milliseconds % 1000) * 1_000_000)
    )
    var remaining = timespec()
    while Darwin.nanosleep(&request, &remaining) != 0 {
        if errno == EINTR {
            request = remaining
            continue
        }
        throw BaselineError.posix("nanosleep", errno)
    }
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
            throw BaselineError.posix("write", errno)
        }
        written += n
    }
}

private func waitForReadable(fd: Int32, deadline: Date) throws {
    while true {
        let remaining = deadline.timeIntervalSinceNow
        if remaining <= 0 {
            throw BaselineError.timeout("timed out waiting for guest exit frame")
        }
        let timeoutMS = Int32(max(1, min(remaining * 1000, Double(Int32.max))))
        var pfd = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
        let n = Darwin.poll(&pfd, 1, timeoutMS)
        if n < 0 {
            if errno == EINTR {
                continue
            }
            throw BaselineError.posix("poll", errno)
        }
        if n == 0 {
            throw BaselineError.timeout("timed out waiting for guest exit frame")
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
            throw BaselineError.posix("read", errno)
        }
        if n == 0 {
            throw BaselineError.vm("guest connection closed before exit frame")
        }
        buffer.append(contentsOf: chunk[0..<n])
    }
}

private func buildVM(opts: Options, queue: DispatchQueue, excludedVirtualizationPIDs: Set<pid_t>) throws -> VMHandle {
    guard VZVirtualMachine.isSupported else {
        throw BaselineError.vm("Virtualization.framework is not supported on this host")
    }

    let bootLoader = VZLinuxBootLoader(kernelURL: URL(fileURLWithPath: opts.kernelPath))
    bootLoader.commandLine = opts.bootArgs
    if !opts.initrdPath.isEmpty {
        bootLoader.initialRamdiskURL = URL(fileURLWithPath: opts.initrdPath)
    }

    let config = VZVirtualMachineConfiguration()
    config.bootLoader = bootLoader
    config.cpuCount = opts.vcpus
    config.memorySize = opts.memoryMiB * 1024 * 1024

    let serial = VZVirtioConsoleDeviceSerialPortConfiguration()
    serial.attachment = try VZFileSerialPortAttachment(
        url: URL(fileURLWithPath: opts.consoleLogPath),
        append: false
    )
    config.serialPorts = [serial]

    if !opts.rootFSPath.isEmpty {
        let disk = try VZDiskImageStorageDeviceAttachment(
            url: URL(fileURLWithPath: opts.rootFSPath),
            readOnly: false
        )
        config.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: disk)]
    }
    config.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    if opts.balloonDevice {
        config.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
    }
    config.socketDevices = [VZVirtioSocketDeviceConfiguration()]

    try config.validate()
    return VMHandle(
        vm: VZVirtualMachine(configuration: config, queue: queue),
        queue: queue,
        excludedVirtualizationPIDs: excludedVirtualizationPIDs
    )
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
        throw BaselineError.timeout("timed out waiting for VM start")
    }
    if let startError {
        throw BaselineError.vm("VM start failed: \(startError)")
    }
}

private func connectVsock(handle: VMHandle, port: UInt32, timeoutSeconds: Double, intervalMS: useconds_t) throws -> VZVirtioSocketConnection {
    guard let socketDevice = handle.vm.socketDevices.first as? VZVirtioSocketDevice else {
        throw BaselineError.vm("VM has no virtio socket device")
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
        throw BaselineError.timeout("timed out connecting to guest vsock port \(port): \(lastError)")
    }
    throw BaselineError.timeout("timed out connecting to guest vsock port \(port)")
}

private struct ProcessMemory {
    let residentSizeBytes: UInt64
    let physFootprintBytes: UInt64
}

private func sampleProcessMemory(pid: pid_t) throws -> ProcessMemory {
    var info = rusage_info_current()
    let rc = withUnsafeMutablePointer(to: &info) { ptr in
        ptr.withMemoryRebound(
            to: rusage_info_t?.self,
            capacity: MemoryLayout<rusage_info_current>.stride / MemoryLayout<rusage_info_t?>.stride
        ) { rebound in
            proc_pid_rusage(pid, RUSAGE_INFO_CURRENT, rebound)
        }
    }
    if rc != 0 {
        throw BaselineError.posix("proc_pid_rusage", errno)
    }
    return ProcessMemory(residentSizeBytes: info.ri_resident_size, physFootprintBytes: info.ri_phys_footprint)
}

private func virtualizationMachinePIDs() -> [pid_t] {
    let process = Process()
    let output = Pipe()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/pgrep")
    process.arguments = ["-f", "com.apple.Virtualization.VirtualMachine"]
    process.standardOutput = output
    process.standardError = FileHandle.nullDevice
    do {
        try process.run()
    } catch {
        return []
    }
    process.waitUntilExit()
    if process.terminationStatus != 0 {
        return []
    }
    let data = output.fileHandleForReading.readDataToEndOfFile()
    let raw = String(data: data, encoding: .utf8) ?? ""
    return raw
        .split(whereSeparator: \.isNewline)
        .compactMap { pid_t(String($0).trimmingCharacters(in: .whitespacesAndNewlines)) }
        .filter { $0 > 0 && $0 != getpid() }
}

private func discoverVirtualizationMachinePIDs(excluding excludedPIDs: Set<pid_t>, timeoutSeconds: Double) -> [pid_t] {
    let deadline = Date().addingTimeInterval(timeoutSeconds)
    var discovered: [pid_t] = []
    repeat {
        discovered = virtualizationMachinePIDs().filter { !excludedPIDs.contains($0) }
        if !discovered.isEmpty {
            return discovered
        }
        usleep(20_000)
    } while Date() < deadline
    return discovered
}

private func sampleHostMemory(label: String, start: UInt64, virtualizationPIDs: [pid_t]) throws -> HostMemorySample {
    let runner = try sampleProcessMemory(pid: getpid())
    let virtualization = virtualizationPIDs.compactMap { pid -> (pid_t, ProcessMemory)? in
        guard let sample = try? sampleProcessMemory(pid: pid) else {
            return nil
        }
        return (pid, sample)
    }
    let virtualizationResident = virtualization.reduce(UInt64(0)) { $0 + $1.1.residentSizeBytes }
    let virtualizationFootprint = virtualization.reduce(UInt64(0)) { $0 + $1.1.physFootprintBytes }
    return HostMemorySample(
        label: label,
        elapsedMS: msSince(start),
        runnerResidentSizeBytes: runner.residentSizeBytes,
        runnerPhysFootprintBytes: runner.physFootprintBytes,
        virtualizationResidentSizeBytes: virtualizationResident,
        virtualizationPhysFootprintBytes: virtualizationFootprint,
        virtualizationPIDs: virtualization.map { $0.0 }
    )
}

private func sampleVirtualMachineHostMemory(label: String, start: UInt64, handle: VMHandle) throws -> HostMemorySample {
    if handle.virtualizationPIDs.isEmpty {
        handle.virtualizationPIDs = discoverVirtualizationMachinePIDs(
            excluding: handle.excludedVirtualizationPIDs,
            timeoutSeconds: 0.5
        )
    }
    var sample = try sampleHostMemory(label: label, start: start, virtualizationPIDs: handle.virtualizationPIDs)
    if sample.virtualizationPIDs.isEmpty {
        handle.virtualizationPIDs = discoverVirtualizationMachinePIDs(
            excluding: handle.excludedVirtualizationPIDs,
            timeoutSeconds: 0.5
        )
        sample = try sampleHostMemory(label: label, start: start, virtualizationPIDs: handle.virtualizationPIDs)
    }
    if sample.virtualizationPIDs.isEmpty {
        throw BaselineError.vm("unable to sample Virtualization.framework helper process for \(label)")
    }
    return sample
}

private func setBalloonTarget(handle: VMHandle, targetMiB: UInt64) throws {
    let sem = DispatchSemaphore(value: 0)
    var targetError: Error?
    handle.queue.async {
        guard let balloon = handle.vm.memoryBalloonDevices.first as? VZVirtioTraditionalMemoryBalloonDevice else {
            targetError = BaselineError.vm("--balloon-target-mib requires --balloon-device on")
            sem.signal()
            return
        }
        balloon.targetVirtualMachineMemorySize = targetMiB * 1024 * 1024
        sem.signal()
    }
    if sem.wait(timeout: .now() + .seconds(1)) == .timedOut {
        throw BaselineError.timeout("timed out setting balloon target to \(targetMiB) MiB")
    }
    if let targetError {
        throw targetError
    }
}

private func probeCommand(opts: Options) -> [String] {
    if opts.probe == "memory-reporting" {
        return [
            "/bin/memprobe",
            "--touch-mib",
            String(opts.probeMemoryMiB),
            "--pre-touch-ms",
            String(opts.probePreTouchMS),
            "--hold-ms",
            String(opts.probeHoldMS),
            "--post-free-ms",
            String(opts.probePostFreeMS),
        ]
    }
    return ["/bin/true"]
}

private func writeExecRequest(connection: VZVirtioSocketConnection, command: [String]) throws {
    let request = ExecRequest(command: command, closedEnv: true)
    let encoded = try JSONEncoder().encode(request)
    try writeAll(fd: connection.fileDescriptor, bytes: Array(encoded) + [0x0A])
}

private func consumeGuestMemoryLines(
    from stdout: inout Data,
    events: inout [GuestMemoryEvent],
    hostSamples: inout [HostMemorySample],
    handle: VMHandle,
    opts: Options,
    start: UInt64,
    balloonTargetApplied: inout Bool
) throws {
    while let newline = stdout.firstIndex(of: 0x0A) {
        let line = stdout.subdata(in: 0..<newline)
        stdout.removeSubrange(0...newline)
        if line.isEmpty {
            continue
        }
        let event = try JSONDecoder().decode(GuestMemoryEvent.self, from: line)
        events.append(event)
        hostSamples.append(try sampleVirtualMachineHostMemory(label: "guest:\(event.phase)", start: start, handle: handle))
        if event.phase == "freed", let targetMiB = opts.balloonTargetMiB, !balloonTargetApplied {
            try setBalloonTarget(handle: handle, targetMiB: targetMiB)
            balloonTargetApplied = true
            hostSamples.append(try sampleVirtualMachineHostMemory(label: "balloon_target:\(targetMiB)mib", start: start, handle: handle))
        }
    }
}

private func probeGuest(
    handle: VMHandle,
    connection: VZVirtioSocketConnection,
    opts: Options,
    start: UInt64
) throws -> ProbeOutcome {
    let deadline = Date().addingTimeInterval(opts.timeoutSeconds)
    var buffer = Data()
    var stdout = Data()
    var guestMemoryEvents: [GuestMemoryEvent] = []
    var hostSamples: [HostMemorySample] = []
    if let targetMiB = opts.preProbeBalloonTargetMiB {
        try setBalloonTarget(handle: handle, targetMiB: targetMiB)
        if opts.preProbeBalloonSettleMS > 0 {
            try sleepMilliseconds(opts.preProbeBalloonSettleMS)
        }
        if opts.probe == "memory-reporting" {
            hostSamples.append(try sampleVirtualMachineHostMemory(label: "balloon_pre_probe:\(targetMiB)mib", start: start, handle: handle))
        }
    }
    try writeExecRequest(connection: connection, command: probeCommand(opts: opts))
    if opts.probe == "memory-reporting" {
        hostSamples.append(try sampleVirtualMachineHostMemory(label: "probe:request_sent", start: start, handle: handle))
    }
    var balloonTargetApplied = false
    while true {
        let line = try readLine(fd: connection.fileDescriptor, buffer: &buffer, deadline: deadline)
        if line.isEmpty {
            continue
        }
        let frame = try JSONDecoder().decode(ExecFrame.self, from: line)
        if opts.probe == "memory-reporting", frame.type == "stdout", let data = frame.data {
            stdout.append(data)
            try consumeGuestMemoryLines(
                from: &stdout,
                events: &guestMemoryEvents,
                hostSamples: &hostSamples,
                handle: handle,
                opts: opts,
                start: start,
                balloonTargetApplied: &balloonTargetApplied
            )
        }
        if frame.type == "exit" {
            return ProbeOutcome(
                exitCode: frame.exitCode ?? 0,
                error: frame.error,
                guestTimingMS: frame.guestTimingMS,
                guestMemoryEvents: guestMemoryEvents,
                hostMemorySamples: hostSamples
            )
        }
    }
}

do {
    let opts = try parseOptions(Array(CommandLine.arguments.dropFirst()))
    let queue = DispatchQueue(label: "cleanroom.benchmark.darwin-vz-minimal.vm")
    let t0 = now()
    let preexistingVirtualizationPIDs = Set(virtualizationMachinePIDs())
    var hostSamples: [HostMemorySample] = []
    if opts.probe == "memory-reporting" {
        hostSamples.append(try sampleHostMemory(label: "host:before_vm", start: t0, virtualizationPIDs: []))
    }
    let handle = try buildVM(opts: opts, queue: queue, excludedVirtualizationPIDs: preexistingVirtualizationPIDs)
    defer { handle.stop() }

    if let targetMiB = opts.initialBalloonTargetMiB {
        try setBalloonTarget(handle: handle, targetMiB: targetMiB)
        if opts.probe == "memory-reporting" {
            hostSamples.append(try sampleHostMemory(label: "balloon_initial:\(targetMiB)mib", start: t0, virtualizationPIDs: []))
        }
    }

    try startVM(handle, timeoutSeconds: opts.timeoutSeconds)
    let startMS = msSince(t0)
    handle.virtualizationPIDs = discoverVirtualizationMachinePIDs(excluding: preexistingVirtualizationPIDs, timeoutSeconds: 2.0)
    if opts.probe == "memory-reporting" {
        hostSamples.append(try sampleVirtualMachineHostMemory(label: "vm:started", start: t0, handle: handle))
    }

    let connection = try connectVsock(
        handle: handle,
        port: opts.guestPort,
        timeoutSeconds: opts.timeoutSeconds,
        intervalMS: opts.connectIntervalMS
    )
    defer { connection.close() }
    let connectMS = msSince(t0)
    if opts.probe == "memory-reporting" {
        hostSamples.append(try sampleVirtualMachineHostMemory(label: "vsock:connected", start: t0, handle: handle))
    }

    let outcome = try probeGuest(handle: handle, connection: connection, opts: opts, start: t0)
    hostSamples.append(contentsOf: outcome.hostMemorySamples)
    if opts.probe == "memory-reporting" {
        hostSamples.append(try sampleVirtualMachineHostMemory(label: "probe:exit", start: t0, handle: handle))
    }
    let execResponseMS = msSince(t0)

    let result = ProbeResult(
        probe: opts.probe,
        startMS: startMS,
        vsockConnectMS: connectMS,
        execResponseMS: execResponseMS,
        probeDurationMS: execResponseMS - connectMS,
        exitCode: outcome.exitCode,
        error: outcome.error,
        guestTimingMS: outcome.guestTimingMS,
        guestMemoryEvents: outcome.guestMemoryEvents.isEmpty ? nil : outcome.guestMemoryEvents,
        hostMemorySamples: hostSamples.isEmpty ? nil : hostSamples,
        vcpus: opts.vcpus,
        memoryMiB: opts.memoryMiB,
        balloonDevice: opts.balloonDevice,
        initialBalloonTargetMiB: opts.initialBalloonTargetMiB,
        preProbeBalloonTargetMiB: opts.preProbeBalloonTargetMiB,
        preProbeBalloonSettleMS: opts.preProbeBalloonSettleMS,
        balloonTargetMiB: opts.balloonTargetMiB
    )
    let encoded = try JSONEncoder().encode(result)
    print(String(data: encoded, encoding: .utf8)!)
    if outcome.exitCode != 0 {
        Foundation.exit(1)
    }
} catch BaselineError.usage(let message) {
    fputs(message + "\n", stderr)
    Foundation.exit(message.hasPrefix("Usage:") ? 0 : 2)
} catch {
    fputs("darwin-vz-minimal: \(error.localizedDescription)\n", stderr)
    Foundation.exit(1)
}

import Darwin
import Foundation
import Virtualization

private struct Options {
    var kernelPath = ""
    var rootFSPath = ""
    var initrdPath = ""
    var bootArgs = ""
    var consoleLogPath = ""
    var guestPort: UInt32 = 10700
    var vcpus = 2
    var memoryMiB: UInt64 = 1024
    var timeoutSeconds = 30.0
    var connectIntervalMS: useconds_t = 10
}

private struct ProbeResult: Encodable {
    let startMS: Double
    let vsockConnectMS: Double
    let execResponseMS: Double
    let probeDurationMS: Double
    let exitCode: Int
    let error: String?
    let guestTimingMS: [String: Int64]?
    let vcpus: Int
    let memoryMiB: UInt64

    enum CodingKeys: String, CodingKey {
        case startMS = "start_ms"
        case vsockConnectMS = "vsock_connect_ms"
        case execResponseMS = "exec_response_ms"
        case probeDurationMS = "probe_duration_ms"
        case exitCode = "exit_code"
        case error
        case guestTimingMS = "guest_timing_ms"
        case vcpus
        case memoryMiB = "memory_mib"
    }
}

private struct ExecFrame: Decodable {
    let type: String
    let exitCode: Int?
    let error: String?
    let guestTimingMS: [String: Int64]?

    enum CodingKeys: String, CodingKey {
        case type
        case exitCode = "exit_code"
        case error
        case guestTimingMS = "guest_timing_ms"
    }
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
      --timeout <seconds>        Start/probe timeout. Default: 30.

    The rootfs mode must already contain Cleanroom's guest runtime. Use a writable throwaway copy.
    The initrd mode expects an /init process that accepts Cleanroom's guest exec JSON protocol on vsock.
    The measured probe boots the VM, connects to the guest agent over vsock, runs /bin/true, and prints JSON timings.
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

private func buildVM(opts: Options, queue: DispatchQueue) throws -> VMHandle {
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
    config.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
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

private func probeGuest(connection: VZVirtioSocketConnection, timeoutSeconds: Double) throws -> (Int, String?, [String: Int64]?) {
    let request = #"{"command":["/bin/true"],"closed_env":true}"# + "\n"
    try writeAll(fd: connection.fileDescriptor, bytes: Array(request.utf8))

    let deadline = Date().addingTimeInterval(timeoutSeconds)
    var buffer = Data()
    while true {
        let line = try readLine(fd: connection.fileDescriptor, buffer: &buffer, deadline: deadline)
        if line.isEmpty {
            continue
        }
        let frame = try JSONDecoder().decode(ExecFrame.self, from: line)
        if frame.type == "exit" {
            return (frame.exitCode ?? 0, frame.error, frame.guestTimingMS)
        }
    }
}

do {
    let opts = try parseOptions(Array(CommandLine.arguments.dropFirst()))
    let queue = DispatchQueue(label: "cleanroom.benchmark.darwin-vz-minimal.vm")
    let t0 = now()
    let handle = try buildVM(opts: opts, queue: queue)
    defer { handle.stop() }

    try startVM(handle, timeoutSeconds: opts.timeoutSeconds)
    let startMS = msSince(t0)

    let connection = try connectVsock(
        handle: handle,
        port: opts.guestPort,
        timeoutSeconds: opts.timeoutSeconds,
        intervalMS: opts.connectIntervalMS
    )
    defer { connection.close() }
    let connectMS = msSince(t0)

    let (exitCode, error, guestTimingMS) = try probeGuest(connection: connection, timeoutSeconds: opts.timeoutSeconds)
    let execResponseMS = msSince(t0)

    let result = ProbeResult(
        startMS: startMS,
        vsockConnectMS: connectMS,
        execResponseMS: execResponseMS,
        probeDurationMS: execResponseMS - connectMS,
        exitCode: exitCode,
        error: error,
        guestTimingMS: guestTimingMS,
        vcpus: opts.vcpus,
        memoryMiB: opts.memoryMiB
    )
    let encoded = try JSONEncoder().encode(result)
    print(String(data: encoded, encoding: .utf8)!)
    if exitCode != 0 {
        Foundation.exit(1)
    }
} catch BaselineError.usage(let message) {
    fputs(message + "\n", stderr)
    Foundation.exit(message.hasPrefix("Usage:") ? 0 : 2)
} catch {
    fputs("darwin-vz-minimal: \(error.localizedDescription)\n", stderr)
    Foundation.exit(1)
}

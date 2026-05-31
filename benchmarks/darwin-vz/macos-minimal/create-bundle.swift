import Foundation
import Virtualization

private struct Options {
    var ipswPath = ""
    var outputPath = ""
    var diskSizeGiB: UInt64 = 120
    var vcpus: Int?
    var memoryMiB: UInt64?
    var agentPort: UInt32 = 10700
    var agentVersion = "uninstalled"
    var displayWidthPx = 1024
    var displayHeightPx = 768
    var displayPixelsPerInch = 72
    var force = false
}

private struct BundleManifest: Encodable {
    let schemaVersion: Int
    let os: String
    let arch: String
    let macOSVersion: String
    let macOSBuild: String
    let vcpus: Int
    let memoryMiB: UInt64
    let disk: String
    let auxiliaryStorage: String
    let hardwareModel: String
    let machineIdentifier: String
    let agent: AgentManifest
    let display: DisplayManifest

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
        case display
    }
}

private struct AgentManifest: Encodable {
    let transport: String
    let port: UInt32
    let version: String
}

private struct DisplayManifest: Encodable {
    let widthPx: Int
    let heightPx: Int
    let pixelsPerInch: Int

    enum CodingKeys: String, CodingKey {
        case widthPx = "width_px"
        case heightPx = "height_px"
        case pixelsPerInch = "pixels_per_inch"
    }
}

private struct BundlePaths {
    let directoryURL: URL
    let manifestURL: URL
    let diskURL: URL
    let auxiliaryStorageURL: URL
    let hardwareModelURL: URL
    let machineIdentifierURL: URL

    init(directoryURL: URL) {
        self.directoryURL = directoryURL
        self.manifestURL = directoryURL.appendingPathComponent("bundle.json")
        self.diskURL = directoryURL.appendingPathComponent("disk.img")
        self.auxiliaryStorageURL = directoryURL.appendingPathComponent("auxiliary.storage")
        self.hardwareModelURL = directoryURL.appendingPathComponent("hardware-model.bin")
        self.machineIdentifierURL = directoryURL.appendingPathComponent("machine-identifier.bin")
    }
}

private enum CreateBundleError: LocalizedError {
    case usage(String)
    case invalid(String)
    case io(String)
    case vm(String)

    var errorDescription: String? {
        switch self {
        case .usage(let message), .invalid(let message), .io(let message), .vm(let message):
            return message
        }
    }
}

private final class ProgressReporter {
    private let progress: Progress
    private let queue = DispatchQueue(label: "cleanroom.benchmark.darwin-vz-macos-bundle.progress")
    private var source: DispatchSourceTimer?
    private var lastPercent = -1

    init(progress: Progress) {
        self.progress = progress
    }

    func start() {
        let source = DispatchSource.makeTimerSource(queue: queue)
        source.schedule(deadline: .now(), repeating: .seconds(5))
        source.setEventHandler { [weak self] in
            self?.printProgress()
        }
        self.source = source
        source.resume()
    }

    func stop() {
        source?.cancel()
        source = nil
    }

    private func printProgress() {
        let percent = Int((progress.fractionCompleted * 100.0).rounded(.down))
        guard percent != lastPercent else {
            return
        }
        lastPercent = percent
        fputs("macOS install progress: \(max(0, min(percent, 100)))%\n", stderr)
    }
}

private func usage() -> String {
    """
    Usage:
      create-bundle --ipsw <UniversalMac.ipsw> --out <bundle-dir> [options]

    Options:
      --disk-size-gib <gib>        Raw disk size. Default: 120.
      --vcpus <count>             vCPU count. Default: max(4, restore image minimum).
      --memory-mib <mib>          Guest memory. Default: max(8192, restore image minimum).
      --agent-port <port>         Guest agent virtio socket port written to bundle.json. Default: 10700.
      --agent-version <version>   Guest agent version written to bundle.json. Default: uninstalled.
      --display <WxH[@ppi]>       Display geometry. Default: 1024x768@72.
      --force                     Replace an existing output directory.
      -h, --help                  Show this help.

    The tool installs macOS from a local Apple Silicon IPSW and writes the
    bundle layout consumed by darwin-vz-macos-minimal. It does not install the
    Cleanroom macOS guest agent inside the guest.
    """
}

private func parseOptions(_ args: [String]) throws -> Options {
    var opts = Options()
    var i = 0
    while i < args.count {
        let arg = args[i]

        func value() throws -> String {
            guard i + 1 < args.count else {
                throw CreateBundleError.usage("missing value for \(arg)\n\n\(usage())")
            }
            i += 1
            return args[i]
        }

        switch arg {
        case "--ipsw":
            opts.ipswPath = try value()
        case "--out":
            opts.outputPath = try value()
        case "--disk-size-gib":
            guard let value = UInt64(try value()), value >= 20 else {
                throw CreateBundleError.invalid("--disk-size-gib must be at least 20")
            }
            opts.diskSizeGiB = value
        case "--vcpus":
            guard let value = Int(try value()), value > 0 else {
                throw CreateBundleError.invalid("--vcpus must be greater than zero")
            }
            opts.vcpus = value
        case "--memory-mib":
            guard let value = UInt64(try value()), value >= 1024 else {
                throw CreateBundleError.invalid("--memory-mib must be at least 1024")
            }
            opts.memoryMiB = value
        case "--agent-port":
            guard let value = UInt32(try value()), value > 0 else {
                throw CreateBundleError.invalid("--agent-port must be greater than zero")
            }
            opts.agentPort = value
        case "--agent-version":
            let value = try value()
            guard !value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                throw CreateBundleError.invalid("--agent-version must not be empty")
            }
            opts.agentVersion = value
        case "--display":
            let display = try parseDisplay(try value())
            opts.displayWidthPx = display.width
            opts.displayHeightPx = display.height
            opts.displayPixelsPerInch = display.ppi
        case "--force":
            opts.force = true
        case "-h", "--help":
            throw CreateBundleError.usage(usage())
        default:
            throw CreateBundleError.usage("unknown argument: \(arg)\n\n\(usage())")
        }
        i += 1
    }

    if opts.ipswPath.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
        throw CreateBundleError.usage("missing --ipsw\n\n\(usage())")
    }
    if opts.outputPath.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
        throw CreateBundleError.usage("missing --out\n\n\(usage())")
    }
    return opts
}

private func parseDisplay(_ input: String) throws -> (width: Int, height: Int, ppi: Int) {
    let displayParts = input.split(separator: "@", omittingEmptySubsequences: false)
    guard displayParts.count == 1 || displayParts.count == 2 else {
        throw CreateBundleError.invalid("--display must use WxH or WxH@ppi")
    }

    let sizeParts = displayParts[0].lowercased().split(separator: "x", omittingEmptySubsequences: false)
    guard sizeParts.count == 2, let width = Int(sizeParts[0]), let height = Int(sizeParts[1]), width > 0, height > 0 else {
        throw CreateBundleError.invalid("--display must use positive WxH dimensions")
    }

    let ppi: Int
    if displayParts.count == 2 {
        guard let value = Int(displayParts[1]), value > 0 else {
            throw CreateBundleError.invalid("--display ppi must be greater than zero")
        }
        ppi = value
    } else {
        ppi = 72
    }
    return (width, height, ppi)
}

private func absoluteURL(_ path: String) -> URL {
    URL(fileURLWithPath: NSString(string: path).expandingTildeInPath)
}

private func requireFile(_ url: URL, label: String) throws {
    var isDir: ObjCBool = false
    guard FileManager.default.fileExists(atPath: url.path, isDirectory: &isDir), !isDir.boolValue else {
        throw CreateBundleError.invalid("\(label) does not exist or is not a file: \(url.path)")
    }
}

private func prepareOutputDirectory(_ outputURL: URL, force: Bool) throws -> BundlePaths {
    let fm = FileManager.default
    if fm.fileExists(atPath: outputURL.path), !force {
        throw CreateBundleError.invalid("output directory already exists: \(outputURL.path)")
    }

    let parent = outputURL.deletingLastPathComponent()
    try fm.createDirectory(at: parent, withIntermediateDirectories: true)
    let tempURL = parent.appendingPathComponent(".\(outputURL.lastPathComponent).tmp-\(UUID().uuidString)")
    try fm.createDirectory(at: tempURL, withIntermediateDirectories: false)
    return BundlePaths(directoryURL: tempURL)
}

private func commitOutputDirectory(tempURL: URL, finalURL: URL, force: Bool) throws {
    let fm = FileManager.default
    guard fm.fileExists(atPath: finalURL.path) else {
        try fm.moveItem(at: tempURL, to: finalURL)
        return
    }
    guard force else {
        throw CreateBundleError.invalid("output directory appeared during install: \(finalURL.path)")
    }

    let replacementURL = finalURL.deletingLastPathComponent()
        .appendingPathComponent(".\(finalURL.lastPathComponent).replaced-\(UUID().uuidString)")
    try fm.moveItem(at: finalURL, to: replacementURL)
    do {
        try fm.moveItem(at: tempURL, to: finalURL)
        try? fm.removeItem(at: replacementURL)
    } catch {
        try? fm.moveItem(at: replacementURL, to: finalURL)
        throw error
    }
}

private func createRawDisk(at url: URL, sizeGiB: UInt64) throws {
    guard sizeGiB <= UInt64.max / 1024 / 1024 / 1024 else {
        throw CreateBundleError.invalid("disk size is too large")
    }
    guard FileManager.default.createFile(atPath: url.path, contents: nil) else {
        throw CreateBundleError.io("failed to create disk image: \(url.path)")
    }
    let handle = try FileHandle(forWritingTo: url)
    defer { try? handle.close() }
    try handle.truncate(atOffset: sizeGiB * 1024 * 1024 * 1024)
}

private func loadRestoreImage(from ipswURL: URL) throws -> VZMacOSRestoreImage {
    let sem = DispatchSemaphore(value: 0)
    var loadedImage: VZMacOSRestoreImage?
    var loadError: Error?

    VZMacOSRestoreImage.load(from: ipswURL) { result in
        switch result {
        case .success(let image):
            loadedImage = image
        case .failure(let error):
            loadError = error
        }
        sem.signal()
    }
    sem.wait()

    if let loadError {
        throw CreateBundleError.vm("failed to load IPSW: \(loadError)")
    }
    guard let loadedImage else {
        throw CreateBundleError.vm("failed to load IPSW")
    }
    if #available(macOS 13.0, *), !loadedImage.isSupported {
        throw CreateBundleError.vm("IPSW is not supported on this host")
    }
    return loadedImage
}

private func formatVersion(_ version: OperatingSystemVersion) -> String {
    if version.patchVersion == 0 {
        return "\(version.majorVersion).\(version.minorVersion)"
    }
    return "\(version.majorVersion).\(version.minorVersion).\(version.patchVersion)"
}

private func buildConfiguration(
    paths: BundlePaths,
    requirements: VZMacOSConfigurationRequirements,
    machineIdentifier: VZMacMachineIdentifier,
    vcpus: Int,
    memoryMiB: UInt64,
    displayWidthPx: Int,
    displayHeightPx: Int,
    displayPixelsPerInch: Int
) throws -> VZVirtualMachineConfiguration {
    let config = VZVirtualMachineConfiguration()
    config.bootLoader = VZMacOSBootLoader()
    config.cpuCount = vcpus
    config.memorySize = memoryMiB * 1024 * 1024

    let platform = VZMacPlatformConfiguration()
    platform.hardwareModel = requirements.hardwareModel
    platform.machineIdentifier = machineIdentifier
    platform.auxiliaryStorage = VZMacAuxiliaryStorage(url: paths.auxiliaryStorageURL)
    config.platform = platform

    let graphics = VZMacGraphicsDeviceConfiguration()
    graphics.displays = [
        VZMacGraphicsDisplayConfiguration(
            widthInPixels: displayWidthPx,
            heightInPixels: displayHeightPx,
            pixelsPerInch: displayPixelsPerInch
        )
    ]
    config.graphicsDevices = [graphics]
    config.keyboards = [VZUSBKeyboardConfiguration()]
    config.pointingDevices = [VZUSBScreenCoordinatePointingDeviceConfiguration()]

    let disk = try VZDiskImageStorageDeviceAttachment(
        url: paths.diskURL,
        readOnly: false,
        cachingMode: .automatic,
        synchronizationMode: .full
    )
    config.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: disk)]
    config.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    config.socketDevices = [VZVirtioSocketDeviceConfiguration()]

    try config.validate()
    return config
}

private func installMacOS(vm: VZVirtualMachine, queue: DispatchQueue, ipswURL: URL) throws {
    let sem = DispatchSemaphore(value: 0)
    var installError: Error?
    var reporter: ProgressReporter?

    queue.async {
        let installer = VZMacOSInstaller(virtualMachine: vm, restoringFromImageAt: ipswURL)
        reporter = ProgressReporter(progress: installer.progress)
        reporter?.start()
        installer.install { result in
            reporter?.stop()
            if case .failure(let error) = result {
                installError = error
            }
            sem.signal()
        }
    }

    sem.wait()
    if let installError {
        throw CreateBundleError.vm("macOS install failed: \(installError)")
    }
}

private func writeManifest(
    paths: BundlePaths,
    image: VZMacOSRestoreImage,
    vcpus: Int,
    memoryMiB: UInt64,
    agentPort: UInt32,
    agentVersion: String,
    displayWidthPx: Int,
    displayHeightPx: Int,
    displayPixelsPerInch: Int
) throws {
    let manifest = BundleManifest(
        schemaVersion: 1,
        os: "macos",
        arch: "arm64",
        macOSVersion: formatVersion(image.operatingSystemVersion),
        macOSBuild: image.buildVersion,
        vcpus: vcpus,
        memoryMiB: memoryMiB,
        disk: paths.diskURL.lastPathComponent,
        auxiliaryStorage: paths.auxiliaryStorageURL.lastPathComponent,
        hardwareModel: paths.hardwareModelURL.lastPathComponent,
        machineIdentifier: paths.machineIdentifierURL.lastPathComponent,
        agent: AgentManifest(transport: "virtio_socket", port: agentPort, version: agentVersion),
        display: DisplayManifest(widthPx: displayWidthPx, heightPx: displayHeightPx, pixelsPerInch: displayPixelsPerInch)
    )

    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
    let encoded = try encoder.encode(manifest)
    try (encoded + Data([0x0A])).write(to: paths.manifestURL)
}

private func run() throws {
    let opts = try parseOptions(Array(CommandLine.arguments.dropFirst()))
    guard VZVirtualMachine.isSupported else {
        throw CreateBundleError.vm("Virtualization.framework is not supported on this host")
    }

    let ipswURL = absoluteURL(opts.ipswPath)
    try requireFile(ipswURL, label: "IPSW")
    let outputURL = absoluteURL(opts.outputPath)
    let paths = try prepareOutputDirectory(outputURL, force: opts.force)
    var committed = false
    defer {
        if !committed {
            try? FileManager.default.removeItem(at: paths.directoryURL)
        }
    }

    let image = try loadRestoreImage(from: ipswURL)
    guard let requirements = image.mostFeaturefulSupportedConfiguration else {
        throw CreateBundleError.vm("IPSW has no configuration supported by this host")
    }
    guard requirements.hardwareModel.isSupported else {
        throw CreateBundleError.vm("IPSW hardware model is not supported by this host")
    }

    let minimumVCPUs = Int(requirements.minimumSupportedCPUCount)
    let minimumMemoryMiB = (requirements.minimumSupportedMemorySize + 1024 * 1024 - 1) / 1024 / 1024
    let vcpus = opts.vcpus ?? max(4, minimumVCPUs)
    let memoryMiB = opts.memoryMiB ?? max(8192, minimumMemoryMiB)
    guard vcpus >= minimumVCPUs else {
        throw CreateBundleError.invalid("--vcpus \(vcpus) is below restore image minimum \(minimumVCPUs)")
    }
    guard memoryMiB <= UInt64.max / 1024 / 1024 else {
        throw CreateBundleError.invalid("--memory-mib is too large")
    }
    guard memoryMiB >= minimumMemoryMiB else {
        throw CreateBundleError.invalid("--memory-mib \(memoryMiB) is below restore image minimum \(minimumMemoryMiB)")
    }

    fputs("creating macOS bundle in \(outputURL.path)\n", stderr)
    fputs("restore image: macOS \(formatVersion(image.operatingSystemVersion)) build \(image.buildVersion)\n", stderr)
    fputs("guest resources: \(vcpus) vCPUs, \(memoryMiB) MiB memory, \(opts.diskSizeGiB) GiB disk\n", stderr)

    _ = try VZMacAuxiliaryStorage(creatingStorageAt: paths.auxiliaryStorageURL, hardwareModel: requirements.hardwareModel)
    try createRawDisk(at: paths.diskURL, sizeGiB: opts.diskSizeGiB)
    let machineIdentifier = VZMacMachineIdentifier()
    try requirements.hardwareModel.dataRepresentation.write(to: paths.hardwareModelURL)
    try machineIdentifier.dataRepresentation.write(to: paths.machineIdentifierURL)

    let queue = DispatchQueue(label: "cleanroom.benchmark.darwin-vz-macos-bundle.install")
    let config = try buildConfiguration(
        paths: paths,
        requirements: requirements,
        machineIdentifier: machineIdentifier,
        vcpus: vcpus,
        memoryMiB: memoryMiB,
        displayWidthPx: opts.displayWidthPx,
        displayHeightPx: opts.displayHeightPx,
        displayPixelsPerInch: opts.displayPixelsPerInch
    )
    let vm = VZVirtualMachine(configuration: config, queue: queue)
    try installMacOS(vm: vm, queue: queue, ipswURL: ipswURL)
    try writeManifest(
        paths: paths,
        image: image,
        vcpus: vcpus,
        memoryMiB: memoryMiB,
        agentPort: opts.agentPort,
        agentVersion: opts.agentVersion,
        displayWidthPx: opts.displayWidthPx,
        displayHeightPx: opts.displayHeightPx,
        displayPixelsPerInch: opts.displayPixelsPerInch
    )
    try commitOutputDirectory(tempURL: paths.directoryURL, finalURL: outputURL, force: opts.force)
    committed = true

    fputs("wrote bundle: \(outputURL.appendingPathComponent("bundle.json").path)\n", stderr)
    fputs("note: install the Cleanroom macOS guest agent before running darwin-vz-macos-minimal against this bundle\n", stderr)
}

do {
    try run()
} catch CreateBundleError.usage(let message) {
    fputs(message + "\n", stderr)
    Foundation.exit(message == usage() ? 0 : 2)
} catch {
    fputs("create-bundle: \(error.localizedDescription)\n", stderr)
    Foundation.exit(1)
}

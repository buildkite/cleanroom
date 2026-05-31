import AppKit
import Foundation
import Virtualization

private struct Options {
    var bundlePath = ""
    var sharedDirectoryPath: String?
    var validateOnly = false
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
        case display
    }
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

private enum ViewerError: LocalizedError {
    case usage(String)
    case invalid(String)
    case vm(String)

    var errorDescription: String? {
        switch self {
        case .usage(let message), .invalid(let message), .vm(let message):
            return message
        }
    }
}

private func usage() -> String {
    """
    Usage:
      darwin-vz-macos-viewer --bundle <bundle.json|bundle-dir> [options]

    Options:
      --shared-directory <path>   Expose a host directory read-only with the macOS guest automount tag.
      --validate-only             Validate the bundle and optional share without starting the VM.
      -h, --help                  Show this help.
    """
}

private func parseOptions(_ args: [String]) throws -> Options {
    var opts = Options()
    var i = 0
    while i < args.count {
        let arg = args[i]

        func value() throws -> String {
            guard i + 1 < args.count else {
                throw ViewerError.usage("missing value for \(arg)\n\n\(usage())")
            }
            i += 1
            return args[i]
        }

        switch arg {
        case "--bundle":
            opts.bundlePath = try value()
        case "--shared-directory":
            opts.sharedDirectoryPath = try value()
        case "--validate-only":
            opts.validateOnly = true
        case "-h", "--help":
            throw ViewerError.usage(usage())
        default:
            throw ViewerError.usage("unknown argument: \(arg)\n\n\(usage())")
        }
        i += 1
    }

    if opts.bundlePath.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
        throw ViewerError.usage("missing --bundle\n\n\(usage())")
    }
    return opts
}

private func absoluteURL(_ path: String, relativeTo baseURL: URL) -> URL {
    let expanded = NSString(string: path).expandingTildeInPath
    if expanded.hasPrefix("/") {
        return URL(fileURLWithPath: expanded)
    }
    return baseURL.appendingPathComponent(expanded)
}

private func fileExists(_ url: URL, description: String) throws {
    var isDirectory = ObjCBool(false)
    guard FileManager.default.fileExists(atPath: url.path, isDirectory: &isDirectory), !isDirectory.boolValue else {
        throw ViewerError.invalid("\(description) does not exist or is a directory: \(url.path)")
    }
}

private func directoryExists(_ url: URL, description: String) throws {
    var isDirectory = ObjCBool(false)
    guard FileManager.default.fileExists(atPath: url.path, isDirectory: &isDirectory), isDirectory.boolValue else {
        throw ViewerError.invalid("\(description) does not exist or is not a directory: \(url.path)")
    }
}

private func resolveBundle(_ path: String) throws -> ResolvedBundle {
    let inputURL = URL(fileURLWithPath: NSString(string: path).expandingTildeInPath)
    var isDirectory = ObjCBool(false)
    guard FileManager.default.fileExists(atPath: inputURL.path, isDirectory: &isDirectory) else {
        throw ViewerError.invalid("bundle path does not exist: \(inputURL.path)")
    }

    let manifestURL = isDirectory.boolValue ? inputURL.appendingPathComponent("bundle.json") : inputURL
    let baseURL = manifestURL.deletingLastPathComponent()
    let data = try Data(contentsOf: manifestURL)
    let decoder = JSONDecoder()
    let manifest = try decoder.decode(BundleManifest.self, from: data)

    guard manifest.schemaVersion == 1 else {
        throw ViewerError.invalid("unsupported schema_version: \(manifest.schemaVersion)")
    }
    guard manifest.os == "macos" else {
        throw ViewerError.invalid("bundle os must be macos")
    }
    guard manifest.arch == "arm64" else {
        throw ViewerError.invalid("bundle arch must be arm64")
    }
    guard manifest.vcpus > 0 else {
        throw ViewerError.invalid("vcpus must be positive")
    }
    guard manifest.memoryMiB > 0 else {
        throw ViewerError.invalid("memory_mib must be positive")
    }

    let diskURL = absoluteURL(manifest.disk, relativeTo: baseURL)
    let auxiliaryStorageURL = absoluteURL(manifest.auxiliaryStorage, relativeTo: baseURL)
    let hardwareModelURL = absoluteURL(manifest.hardwareModel, relativeTo: baseURL)
    let machineIdentifierURL = absoluteURL(manifest.machineIdentifier, relativeTo: baseURL)

    try fileExists(diskURL, description: "disk")
    try fileExists(auxiliaryStorageURL, description: "auxiliary_storage")
    try fileExists(hardwareModelURL, description: "hardware_model")
    try fileExists(machineIdentifierURL, description: "machine_identifier")

    guard let hardwareModel = VZMacHardwareModel(dataRepresentation: try Data(contentsOf: hardwareModelURL)) else {
        throw ViewerError.invalid("hardware_model is not a valid VZMacHardwareModel data representation")
    }
    guard hardwareModel.isSupported else {
        throw ViewerError.invalid("hardware_model is not supported on this host")
    }
    guard let machineIdentifier = VZMacMachineIdentifier(dataRepresentation: try Data(contentsOf: machineIdentifierURL)) else {
        throw ViewerError.invalid("machine_identifier is not a valid VZMacMachineIdentifier data representation")
    }

    return ResolvedBundle(
        manifestURL: manifestURL,
        manifest: manifest,
        diskURL: diskURL,
        auxiliaryStorageURL: auxiliaryStorageURL,
        hardwareModel: hardwareModel,
        machineIdentifier: machineIdentifier
    )
}

private func buildVMConfiguration(bundle: ResolvedBundle, sharedDirectoryPath: String?) throws -> VZVirtualMachineConfiguration {
    guard VZVirtualMachine.isSupported else {
        throw ViewerError.vm("Virtualization.framework is not supported on this host")
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

    if let sharedDirectoryPath {
        guard #available(macOS 13.0, *) else {
            throw ViewerError.vm("--shared-directory requires macOS 13 or newer on the host")
        }
        let currentDirectoryURL = URL(fileURLWithPath: FileManager.default.currentDirectoryPath, isDirectory: true)
        let sharedDirectoryURL = absoluteURL(sharedDirectoryPath, relativeTo: currentDirectoryURL)
        try directoryExists(sharedDirectoryURL, description: "shared directory")

        let sharedDirectory = VZSharedDirectory(url: sharedDirectoryURL, readOnly: true)
        let sharingDevice = VZVirtioFileSystemDeviceConfiguration(tag: VZVirtioFileSystemDeviceConfiguration.macOSGuestAutomountTag)
        sharingDevice.share = VZSingleDirectoryShare(directory: sharedDirectory)
        config.directorySharingDevices = [sharingDevice]
    }

    try config.validate()
    return config
}

private final class ViewerAppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    private var window: NSWindow?
    private var vm: VZVirtualMachine?
    private let queue = DispatchQueue(label: "com.buildkite.cleanroom.macos-viewer.vm")
    private var stopping = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        do {
            let opts = try parseOptions(Array(CommandLine.arguments.dropFirst()))
            let bundle = try resolveBundle(opts.bundlePath)
            let config = try buildVMConfiguration(bundle: bundle, sharedDirectoryPath: opts.sharedDirectoryPath)
            if opts.validateOnly {
                print("bundle metadata validated: \(bundle.manifestURL.path)")
                if let sharedDirectoryPath = opts.sharedDirectoryPath {
                    print("shared directory validated: \(sharedDirectoryPath)")
                }
                exit(0)
            }

            let vm = VZVirtualMachine(configuration: config, queue: queue)
            self.vm = vm

            let display = bundle.manifest.display
            let width = CGFloat(display?.widthPx ?? 1024)
            let height = CGFloat(display?.heightPx ?? 768)
            let window = NSWindow(
                contentRect: NSRect(x: 0, y: 0, width: width, height: height),
                styleMask: [.titled, .closable, .miniaturizable, .resizable],
                backing: .buffered,
                defer: false
            )
            window.title = "Cleanroom macOS VM"
            window.delegate = self

            let view = VZVirtualMachineView(frame: NSRect(x: 0, y: 0, width: width, height: height))
            view.autoresizingMask = [.width, .height]
            view.capturesSystemKeys = true
            if #available(macOS 14.0, *) {
                view.automaticallyReconfiguresDisplay = true
            }
            view.virtualMachine = vm
            window.contentView = view

            if let sharedDirectoryPath = opts.sharedDirectoryPath {
                fputs("shared directory: \(sharedDirectoryPath)\n", stderr)
                fputs("macOS guest mount: /Volumes/My Shared Files\n", stderr)
            }

            self.window = window
            window.center()
            window.makeKeyAndOrderFront(nil)
            NSApp.activate(ignoringOtherApps: true)

            queue.async {
                vm.start { result in
                    if case .failure(let error) = result {
                        DispatchQueue.main.async {
                            fputs("darwin-vz-macos-viewer: VM start failed: \(error)\n", stderr)
                            NSApp.terminate(nil)
                        }
                    }
                }
            }
        } catch ViewerError.usage(let message) {
            print(message)
            exit(message == usage() ? 0 : 64)
        } catch {
            fputs("darwin-vz-macos-viewer: \(error.localizedDescription)\n", stderr)
            exit(1)
        }
    }

    func windowWillClose(_ notification: Notification) {
        NSApp.terminate(nil)
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        stopVM()
        return .terminateNow
    }

    private func stopVM() {
        guard !stopping, let vm else {
            return
        }
        stopping = true
        let sem = DispatchSemaphore(value: 0)
        queue.async {
            if vm.canRequestStop {
                _ = try? vm.requestStop()
            }
            if vm.canStop {
                vm.stop { _ in sem.signal() }
            } else {
                sem.signal()
            }
        }
        _ = sem.wait(timeout: .now() + .seconds(10))
    }
}

let app = NSApplication.shared
private let delegate = ViewerAppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()

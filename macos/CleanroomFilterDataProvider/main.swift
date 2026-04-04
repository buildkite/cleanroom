import Dispatch
import NetworkExtension
import OSLog

private let bootstrapLogger = Logger(
    subsystem: "com.buildkite.cleanroom.network.filter",
    category: "bootstrap"
)

autoreleasepool {
    bootstrapLogger.fault("system extension main entry")
    NEProvider.startSystemExtensionMode()
}

dispatchMain()

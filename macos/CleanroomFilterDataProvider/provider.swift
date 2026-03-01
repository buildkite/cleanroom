import NetworkExtension

@objc(CleanroomFilterDataProvider)
final class CleanroomFilterDataProvider: NEFilterDataProvider {
    override func startFilter(completionHandler: @escaping (Error?) -> Void) {
        // Phase 1 scaffold: provider is present and starts cleanly.
        completionHandler(nil)
    }

    override func stopFilter(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        completionHandler()
    }

    override func handleNewFlow(_ flow: NEFilterFlow) -> NEFilterNewFlowVerdict {
        // Phase 1 scaffold: allow all traffic until per-cleanroom policy wiring is implemented.
        return .allow()
    }
}

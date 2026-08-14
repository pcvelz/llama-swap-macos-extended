import Foundation
import Combine

public final class BackendClient: ObservableObject {
    private let baseURL: URL
    public let bars: [BarMetric]
    private var cancellables = Set<AnyCancellable>()
    private var eventSession: URLSession?

    @Published public var menuState = MenuState() {
        didSet { writeDebugSnapshot() }
    }

    /// Resolves the llama-swap base URL. The parent llama-swap process passes
    /// its own listen address via LLAMA_SWAP_MENU_BASE_URL when it launches
    /// this helper; the fallback matches llama-swap's default listen port.
    /// A value without an http(s) scheme would still parse as a URL (host
    /// becomes the scheme) and silently break every request, so anything
    /// malformed falls back to the default.
    ///
    /// Public because `InstanceGuard` keys its single-instance lock on the same
    /// value: resolving the backend twice by different rules would let two
    /// helpers polling one backend take two different locks.
    public static func defaultBaseURL() -> URL {
        let env = ProcessInfo.processInfo.environment["LLAMA_SWAP_MENU_BASE_URL"] ?? ""
        if env.hasPrefix("http://") || env.hasPrefix("https://"), let url = URL(string: env) {
            return url
        }
        return URL(string: "http://127.0.0.1:8080")!
    }

    public init(baseURL: URL? = nil) {
        self.baseURL = baseURL ?? Self.defaultBaseURL()
        self.bars = BarMetric.parseList(ProcessInfo.processInfo.environment["LLAMA_SWAP_MENU_BARS"])
        // Size the initial bar values to the configured bar count so the icon
        // renders the right number of (empty) bars before the first poll.
        self.menuState.barValues = Array(repeating: 0, count: self.bars.count)
        startPolling()
        startEventSource()
    }

    private func writeDebugSnapshot() {
        guard ProcessInfo.processInfo.environment["LLAMA_MENU_DEBUG_STATE"] == "1" else { return }
        let path = "/tmp/llama-swap-menu-state.json"
        if let data = try? JSONEncoder().encode(menuState),
           let text = String(data: data, encoding: .utf8) {
            try? text.write(toFile: path, atomically: true, encoding: .utf8)
        }
    }

    private func startPolling() {
        Timer.publish(every: 2.0, on: .main, in: .common)
            .autoconnect()
            .sink { [weak self] _ in
                self?.fetchPerformance()
                self?.fetchMetrics()
            }
            .store(in: &cancellables)
    }

    private func fetchPerformance() {
        let url = baseURL.appendingPathComponent("/api/performance")
        URLSession.shared.dataTask(with: url) { [weak self] data, _, _ in
            guard let self, let data else { return }
            if let resp = try? JSONDecoder().decode(PerformanceResponse.self, from: data) {
                let values = self.bars.map { Self.value(for: $0, from: resp) }
                DispatchQueue.main.async {
                    // Mutate via a single assignment so the didSet observer
                    // (debug snapshot writer) fires once per poll, not once
                    // per property.
                    var state = self.menuState
                    state.barValues = values
                    state.backendOnline = true
                    self.menuState = state
                }
            }
        }.resume()
    }

    /// Extracts the latest normalized 0...1 reading for one bar metric.
    private static func value(for metric: BarMetric, from resp: PerformanceResponse) -> Double {
        switch metric {
        case .gpu:
            return (resp.gpuStats.last?.gpuUtilPct ?? 0) / 100.0
        case .vram:
            return (resp.gpuStats.last?.memUtilPct ?? 0) / 100.0
        case .cpu:
            guard let cores = resp.sysStats.last?.cpuUtilPerCore, !cores.isEmpty else { return 0 }
            return cores.reduce(0, +) / Double(cores.count) / 100.0
        case .ram:
            guard let sys = resp.sysStats.last, sys.memTotalMB > 0 else { return 0 }
            return Double(sys.memUsedMB) / Double(sys.memTotalMB)
        }
    }

    private func fetchMetrics() {
        // GET /api/metrics/activity is paginated (sqlite-backed, default page
        // size 25) - counting its "data" array would report a page size, not
        // a true total. /api/metrics/stats carries the aggregate total_requests
        // count directly.
        let url = baseURL.appendingPathComponent("/api/metrics/stats")
        URLSession.shared.dataTask(with: url) { [weak self] data, _, _ in
            guard let data else { return }
            if let stats = try? JSONDecoder().decode(ActivityStats.self, from: data) {
                DispatchQueue.main.async {
                    self?.menuState.completed = stats.totalRequests
                }
            }
        }.resume()
    }

    private func startEventSource() {
        eventSession?.invalidateAndCancel()
        let delegate = EventSourceDelegate { [weak self] data in
            self?.handleEvent(data)
        }
        delegate.reconnect = { [weak self] in
            self?.startEventSource()
        }
        let config = URLSessionConfiguration.default
        config.timeoutIntervalForRequest = TimeInterval.greatestFiniteMagnitude
        let session = URLSession(configuration: config, delegate: delegate, delegateQueue: .main)
        eventSession = session
        let url = baseURL.appendingPathComponent("/api/events")
        var request = URLRequest(url: url)
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        session.dataTask(with: request).resume()
    }

    private final class EventSourceDelegate: NSObject, URLSessionDataDelegate {
        var onEvent: (String) -> Void
        var reconnect: (() -> Void)?
        private var lineBuffer = Data()
        private var eventBuffer = ""

        init(onEvent: @escaping (String) -> Void) {
            self.onEvent = onEvent
        }

        func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
            lineBuffer.append(data)
            while let range = lineBuffer.range(of: Data("\n".utf8)) {
                let lineData = lineBuffer.subdata(in: lineBuffer.startIndex..<range.lowerBound)
                lineBuffer.removeSubrange(lineBuffer.startIndex...range.upperBound.advanced(by: -1))
                var line = String(data: lineData, encoding: .utf8) ?? ""
                if line.hasSuffix("\r") { line.removeLast() }
                if line.hasPrefix("data:") {
                    eventBuffer.append(String(line.dropFirst(5)))
                } else if line.isEmpty {
                    let payload = eventBuffer
                    eventBuffer = ""
                    if !payload.isEmpty {
                        onEvent(payload)
                    }
                }
            }
        }

        func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
            DispatchQueue.main.asyncAfter(deadline: .now() + 2.0) { [weak self] in
                self?.reconnect?()
            }
        }
    }

    private func handleEvent(_ data: String) {
        guard let payload = data.data(using: .utf8),
              let envelope = try? JSONDecoder().decode(EventEnvelope.self, from: payload) else { return }
        switch envelope.type {
        case "modelStatus":
            if let inner = envelope.data.data(using: .utf8),
               let models = try? JSONDecoder().decode([ModelRow].self, from: inner) {
                // Already on main: the SSE session's delegateQueue is .main.
                var state = self.menuState
                state.models = models
                // The switch is done the moment the chosen model is serving.
                if let pending = state.pendingModelID,
                   models.first(where: { $0.id == pending })?.state == "ready" {
                    state.pendingModelID = nil
                    state.lastSwitchError = nil
                }
                self.menuState = state
            }
        case "inflight":
            if let inner = envelope.data.data(using: .utf8),
               let stats = try? JSONDecoder().decode(InFlightStats.self, from: inner) {
                menuState.applyInflight(total: stats.total, byTier: stats.byTier ?? [:])
            }
        default:
            break
        }
    }

    public func unloadAll() {
        var request = URLRequest(url: baseURL.appendingPathComponent("/api/models/unload"))
        request.httpMethod = "POST"
        URLSession.shared.dataTask(with: request) { _, _, _ in }.resume()
    }

    /// Switches the backend to `modelID`. This is the menu's click handler:
    /// `MenuView`'s per-model Button calls exactly this.
    ///
    /// A bare `POST /upstream/<model>/` is not enough: it enters the scheduler as
    /// ordinary traffic, and llama-swap will not evict a model that still has
    /// in-flight requests or is inside its swap-grace window, so on a busy backend
    /// it parks in the queue until the client timeout and nothing changes. Picking
    /// a model is a command, so the incumbent is unloaded first - which does cancel
    /// its in-flight work, the same as "Unload All".
    public func load(modelID: String) {
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            var state = self.menuState

            // Already serving: nothing to switch. Returning here also protects a
            // second resident model (pin/keep-warm) from being evicted as the
            // "incumbent" of a no-op click.
            guard state.models.first(where: { $0.id == modelID })?.state != "ready" else {
                state.chosenModelID = modelID
                self.menuState = state
                return
            }

            state.chosenModelID = modelID
            state.pendingModelID = modelID
            state.lastSwitchError = nil
            self.menuState = state
            self.armSwitchTimeout(for: modelID)

            let incumbent = state.models.first {
                $0.id != modelID && ($0.state == "ready" || $0.state == "starting")
            }?.id

            guard let incumbent else {
                self.postUpstreamLoad(modelID)
                return
            }
            self.unload(modelID: incumbent) { [weak self] in
                // A newer click supersedes this one; without the guard both loads
                // race, and a switch back to `incumbent` would be undone here.
                guard let self, self.menuState.pendingModelID == modelID else { return }
                self.postUpstreamLoad(modelID)
            }
        }
    }

    /// Fails the switch if the model never reports ready. Covers a load that is
    /// accepted at the HTTP level but whose process crash-loops or stalls, which
    /// would otherwise leave the half-bullet up forever.
    private func armSwitchTimeout(for modelID: String) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 300) { [weak self] in
            guard let self, self.menuState.pendingModelID == modelID else { return }
            self.menuState.pendingModelID = nil
            self.menuState.lastSwitchError = "\(modelID) did not become ready"
        }
    }

    /// Unloads one model. Synchronous server-side: llama-swap has stopped the
    /// process by the time this responds, which is what frees the follow-up load
    /// to start instead of parking behind the incumbent.
    private func unload(modelID: String, completion: @escaping () -> Void) {
        var request = URLRequest(url: baseURL.appendingPathComponent("/api/models/unload/\(modelID)"))
        request.httpMethod = "POST"
        request.timeoutInterval = 120
        URLSession.shared.dataTask(with: request) { _, _, _ in
            // Proceed even if the unload failed - the load is still worth trying,
            // and the pending marker surfaces a switch that never completes.
            DispatchQueue.main.async { completion() }
        }.resume()
    }

    /// Triggers the load. POST because GET /upstream/<model>/ answers 503 while the
    /// model is stopped (a guard against health-pollers eager-reloading it).
    ///
    /// The status code is NOT the success signal: llama-server's root handler
    /// answers this empty POST with 404/415 even on a successful load. Only
    /// transport failures are reported; success is decided by the modelStatus
    /// stream clearing `pendingModelID`.
    private func postUpstreamLoad(_ modelID: String) {
        var request = URLRequest(url: baseURL.appendingPathComponent("/upstream/\(modelID)/"))
        request.httpMethod = "POST"
        request.timeoutInterval = 300
        URLSession.shared.dataTask(with: request) { [weak self] _, _, error in
            guard let self, let error else { return }
            DispatchQueue.main.async {
                guard self.menuState.pendingModelID == modelID else { return }
                self.menuState.pendingModelID = nil
                self.menuState.lastSwitchError = error.localizedDescription
            }
        }.resume()
    }
}

import Foundation
import Combine

final class BackendClient: ObservableObject {
    private let baseURL: URL
    private var cancellables = Set<AnyCancellable>()
    private var eventSession: URLSession?

    @Published var menuState = MenuState() {
        didSet { writeDebugSnapshot() }
    }

    init(baseURL: URL = URL(string: "http://localhost:8001")!) {
        self.baseURL = baseURL
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
            guard let data else { return }
            if let resp = try? JSONDecoder().decode(PerformanceResponse.self, from: data),
               let last = resp.gpuStats.last {
                DispatchQueue.main.async {
                    self?.menuState.gpuUtil = last.gpuUtilPct / 100.0
                    self?.menuState.gpuMem = last.memUtilPct / 100.0
                    self?.menuState.backendOnline = true
                }
            }
        }.resume()
    }

    private func fetchMetrics() {
        let url = baseURL.appendingPathComponent("/api/metrics")
        URLSession.shared.dataTask(with: url) { [weak self] data, _, _ in
            guard let data else { return }
            if let entries = try? JSONDecoder().decode([ActivityLogEntry].self, from: data) {
                DispatchQueue.main.async {
                    self?.menuState.completed = entries.count
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
                DispatchQueue.main.async {
                    self.menuState.models = models
                    self.menuState.activeModelID = models.first {
                        $0.state == "ready" || $0.state == "starting"
                    }?.id
                }
            }
        case "inflight":
            if let inner = envelope.data.data(using: .utf8),
               let stats = try? JSONDecoder().decode(InFlightStats.self, from: inner) {
                DispatchQueue.main.async {
                    self.menuState.waiting = stats.total
                }
            }
        default:
            break
        }
    }

    func unloadAll() {
        var request = URLRequest(url: baseURL.appendingPathComponent("/api/models/unload"))
        request.httpMethod = "POST"
        URLSession.shared.dataTask(with: request) { _, _, _ in }.resume()
    }

    /// Loads the requested model by calling llama-swap's upstream load endpoint.
    /// Uses POST because GET /upstream/<model>/ returns 503 when the model is not
    /// yet loaded (guard added to prevent external health-pollers from accidentally
    /// triggering eager reloads). A 415 response from llama-server's root is normal
    /// and means the model loaded and served the empty-body POST.
    func load(modelID: String) {
        let url = baseURL.appendingPathComponent("/upstream/\(modelID)/")
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.timeoutInterval = 300
        URLSession.shared.dataTask(with: request) { _, _, _ in }.resume()
    }
}

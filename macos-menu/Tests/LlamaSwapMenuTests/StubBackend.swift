import Foundation
import Network

/// A minimal in-process HTTP/1.1 stand-in for llama-swap, used to drive
/// `BackendClient` over a real socket. Going through the real network stack
/// (rather than stubbing `URLProtocol`) keeps the client's own URL building,
/// redirect following, and SSE parsing in the test path - those are exactly the
/// layers a model-switch bug can hide in.
final class StubBackend {

    struct Recorded: Equatable {
        let method: String
        let path: String
    }

    /// Decides the reply for a non-SSE request. Returns (status, body).
    /// A negative status means "park": the request is recorded and the
    /// connection held open with no reply, reproducing a load request stuck in
    /// llama-swap's scheduler queue behind a busy incumbent.
    var responder: (_ method: String, _ path: String) -> (Int, String) = { _, _ in (200, "{}") }

    private let listener: NWListener
    private let queue = DispatchQueue(label: "stub-backend")
    private let lock = NSLock()
    private var _recorded: [Recorded] = []
    private var eventConnections: [NWConnection] = []
    private var parkedConnections: [NWConnection] = []

    private(set) var port: UInt16 = 0

    var recorded: [Recorded] {
        lock.lock(); defer { lock.unlock() }
        return _recorded
    }

    var baseURL: URL { URL(string: "http://127.0.0.1:\(port)")! }

    /// True once a client has opened the `/api/events` stream. Pushing before
    /// that races the connect and the event is silently dropped.
    var hasEventClient: Bool {
        lock.lock(); defer { lock.unlock() }
        return !eventConnections.isEmpty
    }

    init() throws {
        let params = NWParameters.tcp
        params.allowLocalEndpointReuse = true
        listener = try NWListener(using: params, on: .any)

        let ready = DispatchSemaphore(value: 0)
        listener.stateUpdateHandler = { [weak self] state in
            if case .ready = state {
                self?.port = self?.listener.port?.rawValue ?? 0
                ready.signal()
            }
        }
        listener.newConnectionHandler = { [weak self] conn in
            self?.accept(conn)
        }
        listener.start(queue: queue)
        _ = ready.wait(timeout: .now() + 5)
    }

    func stop() {
        listener.cancel()
        lock.lock()
        let conns = eventConnections + parkedConnections
        eventConnections = []
        parkedConnections = []
        lock.unlock()
        conns.forEach { $0.cancel() }
    }

    /// Pushes one SSE envelope to every connected `/api/events` listener.
    /// `inner` is the JSON payload llama-swap nests as a *string* inside the
    /// envelope's `data` field, matching the real wire format.
    func pushEvent(type: String, inner: String) {
        let envelope = ["type": type, "data": inner]
        guard let json = try? JSONSerialization.data(withJSONObject: envelope),
              let text = String(data: json, encoding: .utf8) else { return }
        let frame = "data: \(text)\n\n"
        lock.lock()
        let conns = eventConnections
        lock.unlock()
        for conn in conns {
            conn.send(content: Data(frame.utf8), completion: .contentProcessed { _ in })
        }
    }

    /// Convenience: push a `modelStatus` event built from (id, state) pairs.
    func pushModelStatus(_ models: [(id: String, state: String)]) {
        let rows = models.map { m in
            """
            {"id":"\(m.id)","name":"\(m.id)","description":"","state":"\(m.state)",\
            "unlisted":false,"peerID":null,"aliases":["\(m.id)"],"capabilities":null}
            """
        }
        pushEvent(type: "modelStatus", inner: "[\(rows.joined(separator: ","))]")
    }

    // MARK: - connection handling

    private func accept(_ conn: NWConnection) {
        conn.start(queue: queue)
        receiveRequest(conn, buffer: Data())
    }

    private func receiveRequest(_ conn: NWConnection, buffer: Data) {
        conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, isComplete, error in
            guard let self else { return }
            var buf = buffer
            if let data { buf.append(data) }
            if error != nil { conn.cancel(); return }

            guard let headerEnd = buf.range(of: Data("\r\n\r\n".utf8)) else {
                if isComplete { conn.cancel(); return }
                self.receiveRequest(conn, buffer: buf)
                return
            }

            let headerText = String(data: buf[buf.startIndex..<headerEnd.lowerBound], encoding: .utf8) ?? ""
            let lines = headerText.components(separatedBy: "\r\n")
            let parts = (lines.first ?? "").split(separator: " ")
            guard parts.count >= 2 else { conn.cancel(); return }
            let method = String(parts[0])
            let path = String(parts[1])

            // Drain any declared body before replying so the client's write
            // completes cleanly (a POST with a body must not be half-read).
            let contentLength = lines
                .first { $0.lowercased().hasPrefix("content-length:") }
                .flatMap { Int($0.split(separator: ":")[1].trimmingCharacters(in: .whitespaces)) } ?? 0
            let bodySoFar = buf.count - (headerEnd.upperBound - buf.startIndex)
            if bodySoFar < contentLength {
                self.receiveRequest(conn, buffer: buf)
                return
            }

            self.handle(conn, method: method, path: path)
        }
    }

    private func handle(_ conn: NWConnection, method: String, path: String) {
        lock.lock()
        _recorded.append(Recorded(method: method, path: path))
        lock.unlock()

        if path == "/api/events" {
            let head = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n" +
                       "Cache-Control: no-cache\r\nConnection: keep-alive\r\n\r\n"
            conn.send(content: Data(head.utf8), completion: .contentProcessed { _ in })
            lock.lock()
            eventConnections.append(conn)
            lock.unlock()
            return
        }

        let (status, body) = responder(method, path)
        if status < 0 {
            lock.lock()
            parkedConnections.append(conn) // held open with no reply; cancelled by stop()
            lock.unlock()
            return
        }
        let head = "HTTP/1.1 \(status) \(statusText(status))\r\n" +
                   "Content-Type: application/json\r\n" +
                   "Content-Length: \(body.utf8.count)\r\n" +
                   "Connection: close\r\n\r\n"
        conn.send(content: Data((head + body).utf8), completion: .contentProcessed { _ in
            conn.cancel()
        })
    }

    private func statusText(_ code: Int) -> String {
        switch code {
        case 200: return "OK"
        case 404: return "Not Found"
        case 500: return "Internal Server Error"
        case 503: return "Service Unavailable"
        default: return "Status"
        }
    }
}

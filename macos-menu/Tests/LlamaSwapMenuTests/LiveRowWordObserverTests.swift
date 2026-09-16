import XCTest
@testable import LlamaSwapMenuCore

/// Live diagnostic, not a gate: runs the menu's real BackendClient against a
/// running llama-swap and logs every change of a row's word/detail, so a
/// user-reported flicker can be read off the same code path the menu bar
/// renders. Skips unless LLAMA_MENU_LIVE_OBSERVE_URL is set;
/// LLAMA_MENU_LIVE_OBSERVE_SECONDS (default 90) sets the window and
/// LLAMA_MENU_LIVE_OBSERVE_LOG the output file.
final class LiveRowWordObserverTests: XCTestCase {
    func testObserveLiveRowWords() throws {
        let env = ProcessInfo.processInfo.environment
        guard let raw = env["LLAMA_MENU_LIVE_OBSERVE_URL"], let url = URL(string: raw) else {
            throw XCTSkip("set LLAMA_MENU_LIVE_OBSERVE_URL to observe a live backend")
        }
        let seconds = Double(env["LLAMA_MENU_LIVE_OBSERVE_SECONDS"] ?? "") ?? 90
        let logPath = env["LLAMA_MENU_LIVE_OBSERVE_LOG"] ?? "/tmp/llama-menu-live-rows.log"
        FileManager.default.createFile(atPath: logPath, contents: nil)
        let handle = try FileHandle(forWritingTo: URL(fileURLWithPath: logPath))
        defer { try? handle.close() }

        let client = BackendClient(baseURL: url)
        var last: [String: String] = [:]
        let deadline = Date().addingTimeInterval(seconds)
        while Date() < deadline {
            RunLoop.main.run(until: Date().addingTimeInterval(0.25))
            for row in client.menuState.sessionRows {
                let key = row.origin + (row.agent.map { ">" + $0 } ?? "")
                let shown = "\(row.word) | \(row.detail ?? "-")"
                guard last[key] != shown else { continue }
                last[key] = shown
                let line = "\(Date().timeIntervalSince1970) \(key) id=\(row.id) \(shown)\n"
                handle.write(line.data(using: .utf8)!)
            }
        }
    }
}

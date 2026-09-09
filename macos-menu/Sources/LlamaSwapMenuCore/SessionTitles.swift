import Foundation

/// Session titles published by cm-menu, the one component that knows them.
///
/// The proxy answers "which session is this"; it cannot answer "what is that
/// session working on" - the title only exists in the picker. cm-menu
/// therefore rewrites `~/.claude/logs/session-titles.tsv` in full on every
/// refresh, one `<sid8>\t<title>` line per session it knows, and this reader
/// is the only consumer. No title is ever derived from a transcript here.
///
/// A missing or unreadable file is not an error: titles are decoration on a
/// row that must render regardless, so every failure path yields "no title
/// for this session" rather than an empty menu.
public final class SessionTitleStore {
    /// Titles are shown truncated: a menu row has room for an identifier, not
    /// a sentence.
    public static let displayLength = 20

    private let path: String
    private var titles: [String: String] = [:]
    /// Modification date of the file as of the last successful load, so a
    /// poll that finds an unchanged file skips the read entirely - this runs
    /// on every inflight event, which can be several per second.
    private var loadedStamp: Date?

    public static func defaultPath() -> String {
        (NSHomeDirectory() as NSString).appendingPathComponent(".claude/logs/session-titles.tsv")
    }

    public init(path: String? = nil) {
        self.path = path ?? Self.defaultPath()
    }

    /// Re-reads the file when it has changed since the last load. Called once
    /// per poll rather than once per row, so a snapshot of twenty rows costs
    /// at most one stat and one read.
    public func refresh() {
        let attrs = try? FileManager.default.attributesOfItem(atPath: path)
        guard let attrs, let modified = attrs[.modificationDate] as? Date else {
            // File gone (cm-menu not running, or never wrote one): drop what
            // was cached so rows stop showing titles that no longer stand.
            titles = [:]
            loadedStamp = nil
            return
        }
        if let loadedStamp, loadedStamp == modified { return }
        loadedStamp = modified
        titles = Self.parse((try? String(contentsOfFile: path, encoding: .utf8)) ?? "")
    }

    /// Parses the TSV body. Split on the FIRST tab only: a title may contain
    /// tabs, an 8-hex session id may not, so anything after the first
    /// separator belongs to the title.
    public static func parse(_ contents: String) -> [String: String] {
        var result: [String: String] = [:]
        for line in contents.split(separator: "\n", omittingEmptySubsequences: true) {
            guard let tab = line.firstIndex(of: "\t") else { continue }
            let sid = String(line[line.startIndex..<tab]).trimmingCharacters(in: .whitespaces)
            let title = String(line[line.index(after: tab)...]).trimmingCharacters(in: .whitespaces)
            guard !sid.isEmpty, !title.isEmpty else { continue }
            result[sid] = title
        }
        return result
    }

    /// The display title for a session, truncated to displayLength, or nil
    /// when cm-menu knows no title for it. Accepts a full session id or the
    /// short form: the file is keyed by the short one.
    public func title(forSessionID sessionID: String?) -> String? {
        guard let sessionID, !sessionID.isEmpty else { return nil }
        let key = String(sessionID.prefix(8))
        guard let title = titles[key], !title.isEmpty else { return nil }
        return String(title.prefix(Self.displayLength))
    }
}

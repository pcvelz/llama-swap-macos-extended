import Foundation

/// Ensures at most one helper draws a status item for a given backend.
///
/// llama-swap can spawn a helper more than once against the same backend — a
/// redundant `llama-swap` start, or a config reload that relaunches the
/// sidecar. Without a guard every spawn adds another menu-bar icon.
public enum InstanceGuard {

    /// Releasing this descriptor releases the lock, so it is held for the
    /// process lifetime and deliberately never closed on the success path.
    private static var heldDescriptor: Int32 = -1

    /// The lock is keyed on the backend rather than the machine: two llama-swap
    /// instances on different ports are two different backends and each is
    /// entitled to its own icon.
    static func slug(_ baseURL: String) -> String {
        let allowed = CharacterSet.alphanumerics
        return String(baseURL.unicodeScalars.map {
            allowed.contains($0) ? Character($0) : "-"
        })
    }

    public static func lockURL(baseURL: String) -> URL {
        let dir = FileManager.default
            .urls(for: .cachesDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("llama-swap-menu", isDirectory: true)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir.appendingPathComponent("\(slug(baseURL)).lock")
    }

    /// Returns true when this process may draw the status item.
    ///
    /// `waitFor` gives an incumbent that is already on its way out time to
    /// release the lock: when llama-swap restarts, the previous helper can still
    /// hold it for a moment, and the legitimate new helper must not lose the
    /// backend to a corpse.
    @discardableResult
    public static func acquire(baseURL: String, waitFor: TimeInterval = 5.0) -> Bool {
        let fd = open(lockURL(baseURL: baseURL).path, O_CREAT | O_RDWR, 0o644)
        // Filesystem boundary: if the lock file cannot be opened at all, fail
        // open. A missing icon is a worse outcome for the user than a duplicate.
        if fd < 0 { return true }

        let deadline = Date().addingTimeInterval(waitFor)
        repeat {
            if flock(fd, LOCK_EX | LOCK_NB) == 0 {
                heldDescriptor = fd
                return true
            }
            Thread.sleep(forTimeInterval: 0.25)
        } while Date() < deadline

        close(fd)
        return false
    }
}

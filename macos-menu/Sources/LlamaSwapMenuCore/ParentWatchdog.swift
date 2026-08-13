import Foundation

/// Terminates the helper once the llama-swap that spawned it is gone.
///
/// macOS has no `PDEATHSIG`, and llama-swap can leave without stopping the
/// helper — it is SIGKILLed, or it exits via `os.Exit` on the bind-failure path
/// without reaching its shutdown handler. Parent-side cleanup therefore can
/// never be sufficient on its own: a helper that does not reap itself keeps
/// polling the backend and keeps drawing its status item indefinitely.
public enum ParentWatchdog {

    private static var timer: DispatchSourceTimer?

    /// An orphaned process is reparented to launchd, so `getppid() == 1` means
    /// the spawning llama-swap is gone. The helper is never launched by launchd
    /// directly — the LaunchAgent runs llama-swap, which spawns this — so a ppid
    /// of 1 is unambiguous.
    public static func start(interval: TimeInterval = 1.0,
                             onOrphaned: @escaping () -> Void) {
        let t = DispatchSource.makeTimerSource(queue: .main)
        t.schedule(deadline: .now() + interval, repeating: interval)
        t.setEventHandler {
            if getppid() == 1 { onOrphaned() }
        }
        t.resume()
        timer = t
    }
}

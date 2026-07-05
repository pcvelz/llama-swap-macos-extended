import SwiftUI
import AppKit

/// Forces the process to run as an "accessory" (menu-bar agent) application.
///
/// This helper ships as a bare Mach-O executable launched via `exec` by the
/// llama-swap parent — it is NOT an `.app` bundle, so LaunchServices classifies
/// it as `BackgroundOnly` (activation policy *prohibited*) and the SwiftUI
/// `MenuBarExtra` scene silently no-ops: the process runs and polls the backend,
/// but no status item is ever created. Setting `.accessory` here at launch gives
/// the process a UI activation policy regardless of bundle/Info.plist state, so
/// the menu-bar item actually appears.
final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApplication.shared.setActivationPolicy(.accessory)
    }
}

@main
struct LlamaSwapMenuApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @StateObject private var client = BackendClient()

    private var activeDisplayName: String {
        guard let active = client.menuState.activeModelID,
              let model = client.menuState.models.first(where: { $0.id == active }) else {
            return client.menuState.activeModelID ?? ""
        }
        return model.aliases?.first ?? model.name
    }

    var body: some Scene {
        MenuBarExtra {
            MenuView(client: client)
        } label: {
            Image(nsImage: IconRenderer.image(
                modelName: activeDisplayName,
                util: client.menuState.gpuUtil,
                mem: client.menuState.gpuMem
            ))
        }
    }
}

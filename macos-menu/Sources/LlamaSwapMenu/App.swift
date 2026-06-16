import SwiftUI

@main
struct LlamaSwapMenuApp: App {
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

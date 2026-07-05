import SwiftUI

struct MenuView: View {
    @ObservedObject var client: BackendClient

    /// Returns the first alias (e.g. "cq35") when available, otherwise falls back
    /// to the model name or ID so the menu never shows an empty label.
    private func displayName(for model: ModelRow) -> String {
        model.aliases?.first ?? (model.name.isEmpty ? model.id : model.name)
    }

    /// Returns the prefix marker for the active model row.
    /// A filled bullet is used for the currently loaded model; an open bullet
    /// keeps the text aligned for every other model.
    private func bullet(for model: ModelRow) -> String {
        client.menuState.activeModelID == model.id ? "● " : "○ "
    }

    var body: some View {
        let state = client.menuState

        // Section header: read-only, de-emphasized.
        Text("Requests")
            .foregroundStyle(.secondary)
            .disabled(true)

        // Counts are read-only status text (not buttons), so they stay at
        // full opacity for readability.
        Text("\(state.completed) completed")

        Text("\(state.waiting) waiting")

        // Section header: read-only, de-emphasized.
        Text("Model")
            .foregroundStyle(.secondary)
            .disabled(true)

        Divider()

        // One clickable menu item per configured model. Clicking a model calls the
        // same /upstream/<id> endpoint as the web UI's "Load" button. The active
        // model is shown with a filled bullet; inactive models use an open bullet.
        ForEach(state.models) { model in
            Button {
                client.load(modelID: model.id)
            } label: {
                Label {
                    Text(bullet(for: model) + displayName(for: model))
                } icon: {
                    Image(systemName: ModelIcon.sfSymbolName(capabilities: model.capabilities))
                }
            }
        }

        Divider()

        // Only interactive item in the menu.
        Button("Unload All") {
            client.unloadAll()
        }
    }
}

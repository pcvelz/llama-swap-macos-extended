import SwiftUI

extension ModelRow {
    /// First alias (e.g. "cq35") when available, else the name or ID, so a label
    /// is never empty.
    public var displayLabel: String {
        aliases?.first ?? (name.isEmpty ? id : name)
    }
}

public struct MenuView: View {
    public init(client: BackendClient) { self.client = client }

    @ObservedObject var client: BackendClient

    /// Filled bullet = serving; half bullet = the switch the user just started
    /// (loading takes tens of seconds, so without it the click looks ignored);
    /// open bullet keeps the remaining rows aligned.
    private func bullet(for model: ModelRow) -> String {
        if client.menuState.pendingModelID == model.id { return "◐ " }
        return client.menuState.activeModelID == model.id ? "● " : "○ "
    }

    public var body: some View {
        let state = client.menuState

        Text("Requests")
            .foregroundStyle(.secondary)
            .disabled(true)

        // Counts are read-only status text (not buttons), so they stay at
        // full opacity for readability.
        Text("\(state.completed) completed")

        Text("\(state.waiting) waiting")

        Text("Load")
            .foregroundStyle(.secondary)
            .disabled(true)

        // Textual readout of the configured bar metrics, e.g. "GPU 84% · VRAM 62%".
        Text(zip(client.bars, state.barValues)
            .map { "\($0.label) \(Int(($1 * 100).rounded()))%" }
            .joined(separator: " · "))

        Text("Model")
            .foregroundStyle(.secondary)
            .disabled(true)

        Divider()

        // One clickable item per model; clicking switches the backend
        // (see BackendClient.load).
        ForEach(state.models) { model in
            Button {
                client.load(modelID: model.id)
            } label: {
                Label {
                    Text(bullet(for: model) + model.displayLabel)
                } icon: {
                    Image(systemName: ModelIcon.sfSymbolName(capabilities: model.capabilities))
                }
            }
        }

        if let error = state.lastSwitchError {
            Text("Switch failed: \(error)")
                .foregroundStyle(.secondary)
                .disabled(true)
        }

        Divider()

        // Only interactive item in the menu.
        Button("Unload All") {
            client.unloadAll()
        }
    }
}

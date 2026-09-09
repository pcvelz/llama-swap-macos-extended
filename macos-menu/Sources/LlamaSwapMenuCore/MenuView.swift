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

    /// A model's alias/display label, the same one `bullet`'s rows use, so a
    /// cooldown row names models the same way the model list does. Falls
    /// back to the raw id when the model isn't in the current models list
    /// (a transient gap between the swapGrace and modelStatus events).
    private func modelLabel(for id: String, in models: [ModelRow]) -> String {
        models.first(where: { $0.id == id })?.displayLabel ?? id
    }

    public var body: some View {
        let state = client.menuState

        Text("Requests")
            .foregroundStyle(.secondary)
            .disabled(true)

        // Counts are read-only status text (not buttons), so they stay at
        // full opacity for readability.
        Text("\(state.completed) completed")

        Text(state.waitingSummary)

        // One row per in-flight request, in the grammar llama-cm's
        // session-identity contract fixes for both renderers - the row text
        // itself lives on SessionRow.displayLine, so cm-menu and this menu
        // can never disagree about how a request is described.
        ForEach(state.sessionRows) { row in
            Text(row.displayLine)
        }

        // The scheduler's own wait list - "Queue: idle" when nothing is
        // parked, one summary line otherwise (never inferred per-row; see
        // SessionThroughput.swift's header on why PARKED isn't a per-request
        // word here).
        Text(MenuState.queueSummary(state.queueRows))

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

        // One row per active swap-grace hold (llama-cm llama-swap.yaml
        // swapGraceSeconds) - a request parked waiting for a resident model
        // to finish its grace window before it can be evicted. Clicking ends
        // the hold immediately (BackendClient.finishGrace) instead of making
        // the operator wait it out or guess why a model switch is stuck.
        // Hidden entirely when nothing is held - state.graceHolds is empty
        // the overwhelming majority of the time.
        ForEach(state.graceHolds) { hold in
            Button {
                client.finishGrace(reqModel: hold.requestedModel)
            } label: {
                Text("Cooldown: \(modelLabel(for: hold.requestedModel, in: state.models))"
                    + " waiting for \(modelLabel(for: hold.evicteeModel, in: state.models))"
                    + "  (\(CompactFormatter.countdown(hold.remainingSeconds)))")
            }
        }

        Divider()

        // Only interactive item in the menu.
        Button("Unload All") {
            client.unloadAll()
        }
    }
}

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
        // Each row is a Button: clicking evicts the request - a PARKED one
        // drops out of the scheduler's queue, a granted one aborts
        // (BackendClient.cancelInflight -> POST /api/inflight/<id>/cancel).
        ForEach(state.sessionRows) { row in
            Button(row.displayLine) {
                client.cancelInflight(id: row.id)
            }
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
        // The cooldown is a state of the RESIDENT model, so it is rendered on
        // that model's own row ("● cq27 · cooldown 9:41 for [17426df4], then
        // cq35 · 5 waiting") with the slots it protects beneath it, and the
        // row's click ends the cooldown (BackendClient.finishCooldown) rather
        // than reloading a model that is already there. Every other row keeps
        // its load action. No separate cooldown section: the PARKED rows above
        // already say "cooldown", and a block of its own read as a second
        // thing happening (2026-09-10).
        ForEach(state.models) { model in
            if let cd = state.cooldown, cd.evicteeModel == model.id {
                Button {
                    client.finishCooldown()
                } label: {
                    Label {
                        Text(bullet(for: model) + model.displayLabel + " · "
                             + MenuState.cooldownLabel(cd,
                                                       next: modelLabel(for: cd.nextModel, in: state.models),
                                                       restartedAgo: state.cooldownRestartedAt.map { Int(Date().timeIntervalSince($0)) }))
                    } icon: {
                        Image(systemName: ModelIcon.sfSymbolName(capabilities: model.capabilities))
                    }
                }
                ForEach(MenuState.hotSlots(cd)) { slot in
                    Text(MenuState.hotSlotLabel(slot))
                        .disabled(true)
                }
            } else {
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

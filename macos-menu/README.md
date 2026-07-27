# llama-swap-menu

Native macOS menu-bar helper for llama-swap.

## Build

```bash
cd macos-menu
swift build -c release
```

The binary is at `.build/release/llama-swap-menu`.

Sources live in two targets: `LlamaSwapMenuCore` (backend client, state, views) and
`LlamaSwapMenu` (the `@main` app shell). The split exists so tests can drive the
same code the menu runs.

## Test

```bash
make test-mac-menu          # or: cd macos-menu && swift test
```

`ModelSwitchTests` drives `BackendClient.load(modelID:)` — the function the menu's
per-model Button calls — against an in-process HTTP stub.

`LiveModelSwitchTests` does the same against a real llama-swap and requires the
backend's own `/running` to confirm the switch. It is skipped unless enabled,
because it evicts whatever model is currently loaded:

```bash
LLAMA_MENU_LIVE=1 LLAMA_MENU_LIVE_URL=http://localhost:8001 \
LLAMA_MENU_LIVE_TO=cq35 swift test --filter LiveModelSwitchTests
```

It also skips (rather than passing) when the target model is already active, so a
green result always means a switch really happened.

## Run

This helper is normally launched automatically by `llama-swap` when `--menu-bar`
or `menu_bar` is enabled (the default), which passes the backend address and
bar selection via environment variables. To run it manually for development:

```bash
LLAMA_SWAP_MENU_BASE_URL=http://127.0.0.1:8080 \
LLAMA_SWAP_MENU_BARS=gpu,vram \
./.build/release/llama-swap-menu
```

- `LLAMA_SWAP_MENU_BASE_URL` — llama-swap base URL (default `http://127.0.0.1:8080`)
- `LLAMA_SWAP_MENU_BARS` — 1-2 comma-separated bar metrics: `gpu`, `vram`, `cpu`, `ram`
  (default `gpu,vram`); configured via `menu_bar.bars` in llama-swap's config.

Windows and Linux use the Go system-tray equivalent at `cmd/llama-swap-tray`.

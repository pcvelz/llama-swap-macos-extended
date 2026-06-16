# llama-swap-menu

Native macOS menu-bar helper for llama-swap.

## Build

```bash
cd macos-menu
swift build -c release
```

The binary is at `.build/release/llama-swap-menu`.

## Run

This helper is normally launched automatically by `llama-swap` when `--menu-bar`
or `menu_bar: true` is set. To run it manually for development:

```bash
./.build/release/llama-swap-menu
```

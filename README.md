![llama-swap header image](docs/assets/hero3.webp)

# llama-swap (macos-extended fork)

> A fork of [mostlygeek/llama-swap](https://github.com/mostlygeek/llama-swap) that adds a native **menu-bar / system-tray companion** for macOS, Windows and Linux. All credit for llama-swap itself belongs to the upstream project; this fork layers the desktop helper (and a few quality-of-life proxy tweaks) on top.

Run multiple generative AI models on your machine and hot-swap between them on demand. llama-swap works with any OpenAI and Anthropic API compatible server.

Built in Go for performance and simplicity, llama-swap has zero dependencies and is incredibly easy to set up. Get started in minutes - just one binary and one configuration file.

## What this fork adds

A native menu-bar (macOS) / system-tray (Windows, Linux) helper that is **on by default** - start llama-swap and the icon appears next to your clock:

- **Live load bars** in the icon - two configurable bars showing any of: GPU utilization, VRAM, CPU, RAM.
- **Current model at a glance** - the active model's name is shown next to the icon (macOS/Linux) and in the tooltip.
- **Click to switch models** - the menu lists every configured model; clicking one loads it. "Unload All" frees your GPU.
- **Request counters** - completed and waiting request counts, straight from the proxy.

Configure which bars are shown in your regular llama-swap `config.yaml`:

```yaml
menu_bar:
  enabled: true        # default: true (set `menu_bar: false` to opt out)
  bars: [gpu, vram]    # 1-2 of: gpu, vram, cpu, ram (top bar first)
```

GPU stats come from llama-swap's built-in performance monitor (macOS: native; Linux: LACT, nvidia-smi, rocm-smi or sysfs; Windows: nvidia-smi).

## Contributing: run the preflight before you commit

`scripts/preflight.sh` runs locally what GitHub Actions runs remotely. Install it as a pre-commit hook once per clone:

```bash
bash scripts/install-githooks.sh
```

It exists because local verification and CI were running different commands. `make test-all` — what both the Linux and Windows lanes run — is `go test -race`, and a plain `go test ./internal/...` passes on code that CI fails: a data race in a test is invisible without `-race`. A commit verified that way went red on both lanes on 2026-07-27, and the lane had already been red for five days for the same reason.

The preflight mirrors each workflow in `.github/workflows/` and applies the same `paths:` scoping to your changed files, so a docs-only commit costs nothing while a Go change runs the full race suite:

| Lane | Mirrors | Runs |
|---|---|---|
| gofmt | `go-ci.yml` | `gofmt -l .` must be empty |
| build + vet | (fails faster than the suite) | `go build ./...`, `go vet ./internal/...` |
| race suite | `go-ci.yml`, `go-ci-windows.yml` | `make test-all` |
| config schema | `config-schema.yml` | `TestConfig_ExampleMatchesSchema` |
| tray + menubar | `tray-ci.yml` | package tests + linux/amd64, linux/arm64, windows/amd64 cross-builds |
| Swift helper | `tray-ci.yml` (macOS job) | `swift build -c release` + `swift test` |
| UI | `ui-tests.yml` | `make test-ui` |

Run it by hand any time:

```bash
scripts/preflight.sh          # scope to changed files
scripts/preflight.sh --all    # every lane
scripts/preflight.sh --quick  # skip the slow -race suite (does NOT predict CI)
```

**Bypassing is a maintainer decision, not an automated one.** The escape hatches exist for a human who knows why a given commit is safe; an agent or script must fix the failure instead, never route around it:

```bash
LLAMA_SWAP_PREFLIGHT=quick git commit ...   # skip only the -race suite
LLAMA_SWAP_PREFLIGHT=skip  git commit ...   # skip the preflight entirely
```

Two limits worth knowing: it tests the **working tree**, not the staged index (it warns when a file is staged and then further modified), and the Windows-native half of CI cannot run on a non-Windows box — though every failure seen so far reproduced under `-race` on any platform.

## Installation

Each installer downloads the llama-swap binary plus the helper for your platform, and writes a starter config if you don't have one. No package manager, no dependencies.

### macOS (Apple Silicon)

```shell
curl -fsSL https://raw.githubusercontent.com/pcvelz/llama-swap-macos-extended/main/scripts/install-macos.sh | bash
```

Installs `llama-swap` and the native `llama-swap-menu` menu-bar app into `~/bin` and clears the quarantine flag.

### Linux

```shell
curl -fsSL https://raw.githubusercontent.com/pcvelz/llama-swap-macos-extended/main/scripts/install-linux.sh | bash
```

Installs `llama-swap` and `llama-swap-tray` into `~/.local/bin` (amd64 and arm64 supported). The tray needs a StatusNotifierItem/AppIndicator-capable desktop: KDE Plasma and most desktops work out of the box; GNOME needs the [AppIndicator extension](https://extensions.gnome.org/extension/615/appindicator-support/). Headless server? Set `menu_bar: false`.

### Windows (x64)

```powershell
irm https://raw.githubusercontent.com/pcvelz/llama-swap-macos-extended/main/scripts/install-windows.ps1 | iex
```

Installs `llama-swap.exe` and `llama-swap-tray.exe` into `%LOCALAPPDATA%\llama-swap\bin` and adds it to your user PATH.

### Pre-built binaries

All binaries (and the install scripts) are attached to each [release](https://github.com/pcvelz/llama-swap-macos-extended/releases). Grab `llama-swap-<os>-<arch>` plus `llama-swap-menu` (macOS) or `llama-swap-tray-<os>-<arch>` (Windows/Linux) and put them **in the same directory** - llama-swap finds the helper next to its own executable.

### Docker

The upstream project publishes excellent [container images](https://github.com/mostlygeek/llama-swap/pkgs/container/llama-swap) bundling llama-server & friends. They work unchanged, but note the menu/tray helper is a desktop feature and does not apply inside containers.

### Building from source

Requires Go and Node.js (for the web UI); macOS additionally needs Xcode for the menu-bar app.

```shell
git clone https://github.com/pcvelz/llama-swap-macos-extended.git
cd llama-swap-macos-extended
make clean mac      # macOS: llama-swap + menu-bar helper + UI
make linux tray     # Linux binaries + tray helpers
make windows        # Windows binary + tray helper
```

Binaries land in `build/`.

## Getting started

```yaml
# minimum viable config.yaml

models:
  model1:
    cmd: llama-server --port ${PORT} --model /path/to/model.gguf
```

```shell
llama-swap --config config.yaml --listen localhost:8080
```

That's all you need:

1. `models` - holds all model configurations
2. `model1` - the ID used in API calls
3. `cmd` - the command to run to start the server
4. `${PORT}` - an automatically assigned port number

The menu-bar/tray icon appears automatically (disable with `menu_bar: false` or leave off the `--menu-bar` flag when it's disabled in config). The web UI is at `http://localhost:8080/ui`.

Almost all configuration settings are optional and can be added one step at a time:

- Advanced features
  - `matrix` to run concurrent models with a custom swap logic DSL
  - `hooks` to run things on startup
  - `macros` reusable snippets
- Model customization
  - `ttl` to automatically unload models
  - `aliases` to use familiar model names (e.g., "gpt-4o-mini")
  - `env` to pass custom environment variables to inference servers
  - `cmdStop` gracefully stop Docker/Podman containers
  - `useModelName` to override model names sent to upstream servers
  - `${PORT}` automatic port variables for dynamic port assignment
  - `filters` rewrite parts of requests before sending to the upstream server

See the [configuration documentation](docs/configuration.md) for all options.

## Menu bar / system tray reference

| | macOS | Windows | Linux |
|---|---|---|---|
| Helper binary | `llama-swap-menu` (native Swift) | `llama-swap-tray.exe` | `llama-swap-tray` |
| Bars in icon | ✅ | ✅ | ✅ |
| Model name | next to icon | tooltip | next to icon (DE-dependent) + tooltip |
| Model switching menu | ✅ | ✅ | ✅ |
| Requirements | macOS 13+ | Windows 10+ | SNI/AppIndicator host |

**How it launches:** llama-swap starts the helper automatically when `menu_bar` is enabled (the default), looking for the helper binary **next to its own executable**. It passes the listen address and bar selection along, so the helper always points at the right instance - no separate configuration.

**Standalone use:** the tray can also monitor a remote llama-swap:

```shell
llama-swap-tray -base-url http://my-server:8080 -bars cpu,ram
```

(Equivalent environment variables: `LLAMA_SWAP_MENU_BASE_URL`, `LLAMA_SWAP_MENU_BARS`.)

**Bar metrics:** `gpu` (GPU utilization), `vram` (GPU memory), `cpu` (average across cores), `ram` (system memory). First entry is the top bar. If GPU monitoring is unavailable on your system (see `performance` config), pick `cpu`/`ram`.

## Features

- ✅ Easy to deploy and configure: one binary, one configuration file. no external dependencies
- ✅ On-demand model switching
- ✅ Use any local OpenAI compatible server (llama.cpp, vllm, tabbyAPI, stable-diffusion.cpp, etc.)
  - future proof, upgrade your inference servers at any time.
- ✅ OpenAI API supported endpoints:
  - `v1/completions`
  - `v1/chat/completions`
  - `v1/responses`
  - `v1/embeddings`
  - `v1/models` - list available models
  - `v1/audio/speech` ([#36](https://github.com/mostlygeek/llama-swap/issues/36))
  - `v1/audio/transcriptions` ([docs](https://github.com/mostlygeek/llama-swap/issues/41#issuecomment-2722637867))
  - `v1/audio/voices`
  - `v1/images/generations`
  - `v1/images/edits`
- ✅ Anthropic API supported endpoints:
  - `v1/messages`
  - `v1/messages/count_tokens`
- ✅ llama-server (llama.cpp) supported endpoints
  - `v1/rerank`, `v1/reranking`, `/rerank`
  - `/infill` - for code infilling
  - `/completion` - for completion endpoint
- ✅ SDAPI via [stable-diffusion.cpp's server](https://github.com/leejet/stable-diffusion.cpp/tree/master/examples/server)
  - `/sdapi/v1/txt2img`
  - `/sdapi/v1/img2img`
  - `/sdapi/v1/loras` - requires `model` in request body to fetch the correct loras
- ✅ llama-swap API
  - `/ui` - web UI
  - `/upstream/:model_id` - direct access to upstream server ([demo](https://github.com/mostlygeek/llama-swap/pull/31))
  - `/running` - list currently running models ([#61](https://github.com/mostlygeek/llama-swap/issues/61))
  - `POST /api/models/unload` - manually unload all running models ([#58](https://github.com/mostlygeek/llama-swap/issues/58))
  - `POST /api/models/unload/:model_id` - unload a specific model
  - `/logs` - remote log monitoring
    - `GET /logs` returns buffered plain text logs.
      - If `Accept: text/html` is sent, `/logs` redirects to `/ui/`.
    - `GET /logs/stream` keeps the connection open for live log streaming.
      - Stream endpoints send buffered history first by default; add `?no-history` to stream only new lines.
    - `GET /logs/stream/proxy` streams proxy logs only.
    - `GET /logs/stream/upstream` streams upstream process logs only.
    - `GET /logs/stream/{model_id}` streams logs for one model (including IDs with slashes, like `author/model`).
  - `/health` - just returns "OK"
  - `/metrics` - system and GPU metrics for prometheus
- ✅ API Key support - define keys to restrict access to API endpoints
- ✅ Customizable
  - Run concurrent models with a custom DSL swap matrix ([#643](https://github.com/mostlygeek/llama-swap/issues/643))
  - Automatic unloading of models after timeout by setting a `ttl`
  - Docker and Podman support using `cmd` and `cmdStop` together
  - Preload models on startup with `hooks` ([#235](https://github.com/mostlygeek/llama-swap/pull/235))
  - Apply filters to requests to control inference with `stripParams`, `setParams` and `setParamsByID`
  - KV-aware admission for models with a single shared/unified KV pool across parallel slots (`--parallel N --kv-unified`): configure `routing.scheduler.settings.fifo.kvPoolTokens` per model to stop the FIFO scheduler from ever forwarding a request that would push the model's estimated in-flight tokens over the pool, parking it in the queue instead until enough of the pool frees up. Off by default (fail-open) — see `config.example.yaml`.

### Web UI

llama-swap includes a real time web interface with a playground for testing out all sorts of local models:

<img width="1125" height="876" alt="image" src="https://github.com/user-attachments/assets/8ee41947-97af-463d-b0f0-8e9c478fac07" />

View detailed token metrics:

<img width="1111" height="515" alt="image" src="https://github.com/user-attachments/assets/64bfb280-d7a3-4126-971a-a128fd40410c" />

Inspect request and responses:

<img width="1111" height="720" alt="image" src="https://github.com/user-attachments/assets/24fe4aca-1448-4d7c-b9e8-a967589bda6c" />

Manually load and unload models:

<img width="1109" height="719" alt="image" src="https://github.com/user-attachments/assets/02b1e1f2-abd0-4050-84ae-facd66ff01c4" />

Real time log streaming:

<img width="1107" height="559" alt="image" src="https://github.com/user-attachments/assets/39669a10-cff2-409e-836a-5bad8bd0140c" />

## How does llama-swap work?

When a request is made to an OpenAI compatible endpoint, llama-swap will extract the `model` value and load the appropriate server configuration to serve it. If the wrong upstream server is running, it will be replaced with the correct one. This is where the "swap" part comes in. The upstream server is automatically swapped to handle the request correctly.

In the most basic configuration llama-swap handles one model at a time. For more advanced use cases, using a `matrix` allows multiple models to be loaded at the same time. You have complete control over how your system resources are used.

## Reverse Proxy Configuration (nginx)

If you deploy llama-swap behind nginx, disable response buffering for streaming endpoints. By default, nginx buffers responses which breaks Server‑Sent Events (SSE) and streaming chat completion. ([#236](https://github.com/mostlygeek/llama-swap/issues/236))

Recommended nginx configuration snippets:

```nginx
# SSE for UI events/logs
location /api/events {
    proxy_pass http://your-llama-swap-backend;
    proxy_buffering off;
    proxy_cache off;
}

# Streaming chat completions (stream=true)
location /v1/chat/completions {
    proxy_pass http://your-llama-swap-backend;
    proxy_buffering off;
    proxy_cache off;
}
```

As a safeguard, llama-swap also sets `X-Accel-Buffering: no` on SSE responses. However, explicitly disabling `proxy_buffering` at your reverse proxy is still recommended for reliable streaming behavior.

## Monitoring Logs on the CLI

```sh
# sends up to the last 10KB of logs
$ curl http://host/logs

# streams combined logs
curl -Ns http://host/logs/stream

# stream llama-swap's proxy status logs
curl -Ns http://host/logs/stream/proxy

# stream logs from upstream processes that llama-swap loads
curl -Ns http://host/logs/stream/upstream

# stream logs only from a specific model
curl -Ns http://host/logs/stream/{model_id}

# stream and filter logs with linux pipes
curl -Ns http://host/logs/stream | grep 'eval time'

# appending ?no-history will disable sending buffered history first
curl -Ns 'http://host/logs/stream?no-history'
```

## Do I need to use llama.cpp's server (llama-server)?

Any OpenAI compatible server would work. llama-swap was originally designed for llama-server and it is the best supported.

For Python based inference servers like vllm or tabbyAPI it is recommended to run them via podman or docker. This provides clean environment isolation as well as responding correctly to `SIGTERM` signals for proper shutdown.

# Define variables for the application
APP_NAME = llama-swap
BUILD_DIR = build

# Get closest tag or if that fails (no git repo or no tags) then devel
GIT_VERSION := $(shell git describe --abbrev=6 --tags 2>/dev/null || echo devel)
# Get the current Git hash
GIT_HASH := $(shell git rev-parse --short HEAD)
ifneq ($(shell git status --porcelain),)
    # There are untracked changes
    GIT_HASH := $(GIT_HASH)+
endif

# Capture the current build date in RFC3339 format
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Default target: Builds binaries for both OSX and Linux
all: mac linux simple-responder

# Clean build directory
clean:
	rm -rf $(BUILD_DIR)

# use cached test results while developing
test-dev:
	go test -short ./...
	staticcheck ./... || true

test: ensure-simple-responder
	go test -short -count=1 ./internal/...

# for CI - full test (takes longer)
test-all: ensure-simple-responder
	go test -race -count=1 ./internal/...

# internal/process tests silently SKIP (not fail) when the simple-responder
# helper binary is missing, so a bare `make test`/`make test-all` can look
# green while doing nothing. Build it only if it's not already there, so CI's
# cache-restore step (see .github/workflows/go-ci*.yml) is still respected.
ensure-simple-responder:
	@if [ "$$(go env GOOS)" = "windows" ]; then \
		test -f $(BUILD_DIR)/simple-responder.exe || $(MAKE) simple-responder-windows; \
	else \
		test -f $(BUILD_DIR)/simple-responder_$$(go env GOOS)_$$(go env GOARCH) || $(MAKE) simple-responder; \
	fi

ui/node_modules:
	cd ui-svelte && npm install

# build react UI into internal/server/ui_dist; the `embed_ui` build tag embeds
# this output into the binary (see internal/server/embed.go)
ui: ui/node_modules
	cd ui-svelte && npm run build

# Build OSX binary
mac: mac-menu ui
	@echo "Building Mac binary..."
	GOOS=darwin GOARCH=arm64 go build -tags embed_ui -ldflags="-X main.commit=${GIT_HASH} -X main.version=${GIT_VERSION} -X main.date=${BUILD_DATE}" -o $(BUILD_DIR)/$(APP_NAME)-darwin-arm64

mac-menu:
	@echo "Building macOS menu-bar helper..."
	cd macos-menu && swift build -c release
	mkdir -p $(BUILD_DIR)
	cp macos-menu/.build/release/llama-swap-menu $(BUILD_DIR)/llama-swap-menu

# Cross-platform system-tray helper (Windows/Linux; macOS uses macos-menu).
# CGO_ENABLED=0 works because fyne.io/systray is pure Go on these platforms.
tray:
	@echo "Building system-tray helper..."
	mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o $(BUILD_DIR)/llama-swap-tray-windows-amd64.exe ./cmd/llama-swap-tray
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o $(BUILD_DIR)/llama-swap-tray-linux-amd64 ./cmd/llama-swap-tray
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(BUILD_DIR)/llama-swap-tray-linux-arm64 ./cmd/llama-swap-tray

# Build Linux binary
linux: linux-arm64 linux-amd64

linux-amd64: ui tray
	@echo "Building Linux AMD64 binary..."
	GOOS=linux GOARCH=amd64 go build -tags embed_ui -ldflags="-X main.commit=${GIT_HASH} -X main.version=${GIT_VERSION} -X main.date=${BUILD_DATE}" -o $(BUILD_DIR)/$(APP_NAME)-linux-amd64

linux-arm64: ui tray
	@echo "Building Linux ARM64 binary..."
	GOOS=linux GOARCH=arm64 go build -tags embed_ui -ldflags="-X main.commit=${GIT_HASH} -X main.version=${GIT_VERSION} -X main.date=${BUILD_DATE}" -o $(BUILD_DIR)/$(APP_NAME)-linux-arm64

# Build Windows binary
windows: ui tray
	@echo "Building Windows binary..."
	GOOS=windows GOARCH=amd64 go build -tags embed_ui -ldflags="-X main.commit=${GIT_HASH} -X main.version=${GIT_VERSION} -X main.date=${BUILD_DATE}" -o $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe

# for testing with real external processes
simple-responder:
	@echo "Building simple responder"
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/simple-responder_darwin_arm64 cmd/simple-responder/simple-responder.go
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/simple-responder_linux_amd64 cmd/simple-responder/simple-responder.go

simple-responder-windows:
	@echo "Building simple responder for windows"
	GOOS=windows GOARCH=amd64 go build -o $(BUILD_DIR)/simple-responder.exe cmd/simple-responder/simple-responder.go

# Ensure build directory exists
$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

# Create a new release tag
release:
	@echo "Checking for unstaged changes..."
	@if [ -n "$(shell git status --porcelain)" ]; then \
		echo "Error: There are unstaged changes. Please commit or stash your changes before creating a release tag." >&2; \
		exit 1; \
	fi

# Get the highest tag in v{number} format, increment it, and create a new tag
	@highest_tag=$$(git tag --sort=-v:refname | grep -E '^v[0-9]+$$' | head -n 1 || echo "v0"); \
	new_tag="v$$(( $${highest_tag#v} + 1 ))"; \
	echo "tagging new version: $$new_tag"; \
	git tag "$$new_tag";

GOOS ?= $(shell go env GOOS 2>/dev/null || echo linux)
GOARCH ?= $(shell go env GOARCH 2>/dev/null || echo amd64)
wol-proxy: $(BUILD_DIR)
	@echo "Building wol-proxy"
	go build -o $(BUILD_DIR)/wol-proxy-$(GOOS)-$(GOARCH)-$(shell date +%Y-%m-%d) cmd/wol-proxy/wol-proxy.go

test-ui:
	cd ui-svelte && npm ci && npm run check && npm test

# run the full local CI mirror + workflow hygiene lane (see scripts/preflight.sh)
preflight:
	scripts/preflight.sh --all

GIT_SHA ?= $(shell git rev-parse HEAD)
# wait for GitHub Actions on GIT_SHA (default HEAD) to go green, fork-only
ci-await:
	scripts/ci-await.sh "$(GIT_SHA)"

# macOS menu-bar helper unit tests. The live backend test is skipped unless
# LLAMA_MENU_LIVE=1 is set, since it swaps models on a running llama-swap.
test-mac-menu:
	cd macos-menu && swift test

# Phony targets
.PHONY: all clean ui mac mac-menu tray windows simple-responder simple-responder-windows ensure-simple-responder test test-all test-dev test-ui test-mac-menu wol-proxy preflight ci-await
.PHONE: linux linux-arm64 linux-amd64

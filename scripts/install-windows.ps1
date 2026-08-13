# Installer for the llama-swap-macos-extended fork (Windows).
# Downloads the llama-swap binary and the system-tray helper into
# %LOCALAPPDATA%\llama-swap\bin, adds that directory to the user PATH, and
# writes a starter config. The tray helper is launched automatically by
# llama-swap (menu_bar is on by default in this fork).
#
# Run from PowerShell:
#   irm https://github.com/pcvelz/llama-swap-macos-extended/releases/latest/download/install-windows.ps1 | iex

$ErrorActionPreference = "Stop"

$repo = "pcvelz/llama-swap-macos-extended"
$base = "https://github.com/$repo/releases/latest/download"
$dest = Join-Path $env:LOCALAPPDATA "llama-swap\bin"
$configDir = Join-Path $env:LOCALAPPDATA "llama-swap"
$configFile = Join-Path $configDir "config.yaml"

New-Item -ItemType Directory -Force -Path $dest | Out-Null

$assets = @(
    @{ Asset = "llama-swap-windows-amd64.exe";      Target = "llama-swap.exe" },
    @{ Asset = "llama-swap-tray-windows-amd64.exe"; Target = "llama-swap-tray.exe" }
)

foreach ($a in $assets) {
    $target = Join-Path $dest $a.Target
    Write-Host "Downloading $($a.Asset)..."
    Invoke-WebRequest -Uri "$base/$($a.Asset)" -OutFile $target
}

if (-not (Test-Path $configFile)) {
    @"
# llama-swap starter config - add your models below.
# Full reference: https://github.com/pcvelz/llama-swap-macos-extended

models:
  # example:
  #   cmd: llama-server --port `${PORT} --model C:\path\to\model.gguf

# menu_bar: system-tray helper (on by default)
# bars: 1-2 of gpu, vram, cpu, ram
menu_bar:
  enabled: true
  bars: [gpu, vram]
"@ | Set-Content -Path $configFile -Encoding UTF8
    Write-Host "Wrote starter config to $configFile"
}

# Add the bin directory to the user PATH if missing.
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dest*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$dest", "User")
    Write-Host "Added $dest to your user PATH (restart your terminal to pick it up)."
}

Write-Host ""
Write-Host "Installed llama-swap and llama-swap-tray to $dest."
Write-Host "Start with:  llama-swap --config `"$configFile`" --listen localhost:8080"

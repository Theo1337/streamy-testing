param(
    [string]$Version = "0.1.0-dev",
    [string]$Commit = "",
    [string]$BuildDate = ""
)

$ErrorActionPreference = "Stop"

$root = $PSScriptRoot

if (-not $Commit) {
    try {
        $Commit = (git -C $root rev-parse --short HEAD 2>$null).Trim()
    } catch {}
    if (-not $Commit) { $Commit = "unknown" }
}
if (-not $BuildDate) {
    $BuildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
}
$go = Get-Command go -ErrorAction SilentlyContinue
if ($go) {
    $goExe = $go.Source
} else {
    $goExe = Join-Path $env:ProgramFiles "Go\bin\go.exe"
    if (-not (Test-Path $goExe)) {
        throw "Go was not found. Install Go 1.27+ or add it to PATH."
    }
}
$npm = Get-Command npm.cmd -ErrorAction SilentlyContinue
if (-not $npm) {
    throw "Node.js/npm is required to build the Svelte settings UI."
}

$frontend = Join-Path $root "ui\frontend"
Push-Location $frontend
try {
    & $npm.Source ci
    if ($LASTEXITCODE -ne 0) { throw "npm ci failed with exit code $LASTEXITCODE" }
    & $npm.Source run build
    if ($LASTEXITCODE -ne 0) { throw "frontend build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

$output = Join-Path $root "out\bin"
New-Item -ItemType Directory -Force -Path $output | Out-Null
$trayLdflags = "-s -w -H=windowsgui -X streamy/internal/version.Version=$Version -X streamy/internal/version.Commit=$Commit -X streamy/internal/version.Date=$BuildDate"
Push-Location $root
try {
    & $goExe build -ldflags $trayLdflags -o (Join-Path $output "streamy.exe") .\cmd\streamy
    if ($LASTEXITCODE -ne 0) { throw "tray build failed with exit code $LASTEXITCODE" }
    & $goExe build -ldflags "-s -w -H=windowsgui" -o (Join-Path $output "streamy-updater.exe") .\cmd\streamy-updater
    if ($LASTEXITCODE -ne 0) { throw "updater helper build failed with exit code $LASTEXITCODE" }
    & $goExe build -tags "production,desktop" -ldflags "-s -w -H=windowsgui" -o (Join-Path $output "streamy-ui.exe") .\ui
    if ($LASTEXITCODE -ne 0) { throw "settings UI build failed with exit code $LASTEXITCODE" }
    & $goExe build -o (Join-Path $output "streamy-capture.exe") .\cmd\streamy-capture
    if ($LASTEXITCODE -ne 0) { throw "capture diagnostic build failed with exit code $LASTEXITCODE" }
    & $goExe build -o (Join-Path $output "streamy-manual.exe") .\cmd\streamy-manual
    if ($LASTEXITCODE -ne 0) { throw "manual WebRTC tool build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

Write-Host "Build complete: $output"

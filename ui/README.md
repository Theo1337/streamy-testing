# ui/

The on-demand settings and picker window: a separate Wails process with a Svelte
frontend. The tray does not import Wails or load WebView2 while idle. A random,
per-launch token protects the local HTTP IPC server, which binds only to
`127.0.0.1`.

## Contents

| Path | Purpose |
| --- | --- |
| `main.go` | Wails entry point and embedded frontend |
| `app.go` | Wails methods that call authenticated tray IPC endpoints |
| `frontend/` | Svelte settings form, target list and refreshed live previews |
| `../internal/app/ipc.go` | Loopback IPC endpoints, config persistence and target selection |

The picker previews monitors and visible windows, then starts a share on the
selected target. Settings include first-run setup, Discord identity, startup,
hotkeys, capture behavior, resolution, fps,
bitrate, encoder, STUN/TURN, signaling endpoints and viewer count.

## Build

```powershell
.\build.ps1
```

The build script compiles the Svelte frontend, embeds its assets, and builds
Wails with the required `production,desktop` Go build tags. Keep `streamy.exe`
and `streamy-ui.exe` together in `out\bin`. Windows 10/11 needs the Microsoft
Edge WebView2 Runtime, which is not bundled.

# Streamy — screenshare app

A tiny always-on background app that will let you press a global hotkey, pick a
screen or window and stream it live to anyone with a browser link — your own
replacement for Discord Go Live, free to run.

This folder is **the desktop app**. The web side (viewer page + signaling
Worker) lives in the sibling `stream-site/` folder and the two only share the
wire format documented in [`PROTOCOL.md`](PROTOCOL.md).

**Status:** the seven implementation phases are complete. Live WebRTC sharing,
multiple viewers, the separate Wails/Svelte picker and settings window, and the
Worker/viewer deployment paths are implemented. The optional local Discord bot
posts and removes the public viewer link. A real Cloudflare deploy and a
cross-network screen-sharing session still require your accounts and a Windows PC
with ffmpeg.

---

## What works

### The skeleton (phase 1)

| Requirement | Where |
| --- | --- |
| System tray icon with a menu (Start/Stop sharing, Open settings, Copy viewer link, Quit) | `internal/tray/tray.go` |
| Global hotkey listener, configurable, no UI on press | `internal/hotkey/` |
| Tray notifications | `internal/notify/` |
| Settings as JSON in the user config directory, with defaults | `internal/config/config.go` |
| Structured logging (`log/slog`) to stderr and a file | `internal/logging/logging.go` |
| Clean shutdown: hotkey released, pipeline stopped, log flushed | `internal/app/app.go` (`Run`/`shutdown`) |
| No UI framework loaded while idle | Wails is imported only by the separately built `ui` executable |
| Only one instance at a time | `internal/singleinstance/` |
| Tray icons generated, not binary blobs | `tools/genicon/` → `internal/tray/assets/` |

### The capture pipeline (phase 2)

| Requirement | Where |
| --- | --- |
| Screen capture via Desktop Duplication (`ddagrab`), with a GDI fallback (`gdigrab`) | `internal/capture/ffmpeg.go` |
| Deciding *what* to capture, as a pure testable policy | `internal/capture/target.go` (`Decide`) |
| Window, monitor and DXGI output enumeration | `internal/capture/win_*.go` |
| ffmpeg discovery, capability probing, encoder choice | `internal/encode/` |
| The ffmpeg subprocess: arguments, graceful stop, output tail on failure | `internal/encode/runner.go` |
| The app-facing pipeline: resolve → start → record → stop | `internal/pipeline/recorder.go` |
| A debugging CLI for all of the above | `cmd/streamy-capture/` |
| Rotation (portrait monitors) handled with a `transpose` filter | `internal/capture/ffmpeg.go` |

### Manual WebRTC test (phase 3)

| Requirement | Where |
| --- | --- |
| One Pion peer connection sends H.264 to a browser | `internal/rtc/sender.go` |
| Annex-B NAL units grouped into frames before sending | `internal/rtc/annexb.go` |
| Local test viewer page served without another runtime | `cmd/streamy-manual/` |
| Responsive manual-offer/answer test page | `../stream-site/viewer/` |

### Room signaling and live sharing (phase 4)

| Requirement | Where |
| --- | --- |
| Random room IDs and secret tokens stored only in the URL fragment | `internal/signal/` |
| Room-authenticated Worker WebSocket client | `internal/signal/signal.go` |
| Durable Object auth, peer relay, viewer caps and rate limiting | `../stream-site/worker/src/` |
| Tray Start/Stop, clipboard and notification flow uses live WebRTC | `internal/pipeline/livestream.go` |
| Shared public lobby link does not expose the host token; active lobby records include viewer ICE settings | `internal/signal/signal.go`, `../stream-site/worker/src/lobby.js` |
| Multiple viewers share one encoded H.264 track and keyframe requests resync at IDR | `internal/rtc/broadcaster.go` |
| Upload bandwidth estimate and viewer count logging | `internal/pipeline/livestream.go` |

### Picker and settings UI (phase 6)

The tray starts `streamy-ui.exe` only when settings or the picker is requested.
That separate Wails process uses a Svelte window with live display/window
previews, settings forms, and a short-lived token-authenticated HTTP channel
bound only to `127.0.0.1`. The tray itself imports no Wails or frontend runtime.
The picker can start a specific display/window, and the hotkey settings can
toggle sharing or open the picker.

### Live indicator

While a stream is live, Streamy can draw a small blinking red dot in one of the
four corners of the primary display. It is a hand-written layered Win32 window
(`internal/liveindicator`): always on top, click-through, never takes focus, and
kept out of screen capture with `WDA_EXCLUDEFROMCAPTURE`, so only the streamer
sees it. It is created only when the first stream starts; disable it or change
its corner in **Settings → General → Live indicator**. As with any normal
desktop window, a truly exclusive fullscreen DirectX app can still cover it;
borderless fullscreen and windowed apps cannot.

### Discord bot notification

| Requirement | Where |
| --- | --- |
| `Output` interface for the encoded media path | `internal/output/output.go` |
| WebRTC output wrapping the shared `rtc.Broadcaster` | `internal/output/webrtc.go` |
| HTTP notification that posts the live viewer link | `internal/output/notify.go` |
| Bot lifecycle and idle shutdown | `internal/stack/stack.go`, `internal/app/app.go` |
| Channel, notify URL and notify key settings | `internal/config/config.go` |

### Release polish (phase 7)

The viewer provides a public active-stream lobby, automatically opens the first
stream muted, and offers custom playback, volume, and fullscreen controls.
Streamers connect Discord in Settings so their display name and avatar can be
listed publicly; viewer names are not collected. Use `build.ps1` for the four
Windows executables; Worker deployment and Pages instructions live in
`../stream-site/`.

Pressing the hotkey starts sharing, copies the room link, and shows it in the
tray notification. Press it again to stop capture, WebRTC and signaling.
Recording remains available with `-record-only`.

## Requirements

- **Go 1.27 or newer** (`go version`).
- The tray has no Node, Python or C compiler runtime requirement. Node/npm are
  needed only to build the Svelte settings UI; the UI then ships as embedded
  static assets. Wails uses the installed Windows WebView2 runtime and does not
  bundle Chromium.
- **ffmpeg**, needed from phase 2 onwards. The app finds it on `PATH`, or reads
  `encoding.ffmpegPath`. It is used only as a subprocess; nothing links against
  libav. Live desktop audio uses native Windows process-loopback capture and
  needs an FFmpeg build with the `libopus` encoder; Streamy checks for it at
  startup. FFmpeg does not need a WASAPI input device.

```powershell
winget install Gyan.FFmpeg     # or any other ffmpeg build with h264 encoders
ffmpeg -version
```

## Build and run

```powershell
.\build.ps1                 # full Windows release build, including Svelte UI

go run ./cmd/streamy            # run in the foreground; Ctrl+C to quit
go run ./cmd/streamy -headless  # no tray icon, logs everything (best for debugging)
go run ./cmd/streamy -record-only # record to MP4 instead of sharing
go run ./cmd/streamy-manual     # phase 3: manually connect the browser test page

go build -o streamy.exe ./cmd/streamy
go build -o streamy-capture.exe ./cmd/streamy-capture   # the debugging CLI (phase 2)
go build -o streamy-manual.exe ./cmd/streamy-manual
go test ./...
go vet ./...
```

For a built app, keep `streamy.exe` and `streamy-ui.exe` together in
`out\bin`; start `streamy.exe`. The tray process remains separate from the
on-demand UI process. Windows 10/11 must have the Microsoft Edge WebView2
Runtime installed to display settings. For source runs, first build with
`.\build.ps1`, then run `go run ./cmd/streamy` from the repository root; the
tray locates the UI in `out\bin` even though `go run` itself uses a temporary
executable directory.

### Configure live browser sharing

Deploy the Worker and viewer page as described in `stream-site/`, and configure
the Worker’s Discord OAuth client ID and secrets. Set these fields in
`%AppData%\streamy\config.json`:

```json
{
  "signaling": {
    "url": "wss://streamy-signal.thenightserver.workers.dev",
    "viewerURL": "https://streamy-viewer.pages.dev/"
  }
}
```

These deployment URLs are bundled defaults for this Streamy build and are not
exposed in the settings window. Edit `config.json` if the Worker or Pages
deployment moves. Install ffmpeg, run `go run ./cmd/streamy`, connect Discord in Settings, then
use the tray or hotkey to start a share. The app creates a fresh room and
separate viewer token for every session; the video itself is peer-to-peer. For
a local Worker use `ws://127.0.0.1:8788`; the local viewer is served at
`http://127.0.0.1:8787/`.
If `signaling.url` is empty, Start reports the configuration error rather than
starting a live session; configure the deployed Worker URL before sharing.

Desktop audio capture is enabled by default and can be switched off in Settings.
Streamy uses Windows process-loopback capture to capture the system mix while
excluding the Discord desktop app's process tree. The Discord app must be
running when sharing starts; the setting falls back to the full system mix
with a warning if Discord is not found. This exclusion requires Windows 10
build 20348 or later and applies to the installed Discord desktop app, not
Discord in a browser. If multiple separate Discord client trees are running,
close extras so Streamy can exclude the correct tree. Viewers start muted for
autoplay, so they must unmute in the viewer controls (particularly on mobile).
Capture and Opus packetization add a small unavoidable delay; zero-latency
audio is not physically achievable.

On first launch, Streamy opens a setup wizard for Discord sign-in, the default
capture target, and the optional Windows startup setting. The signaling and
viewer service URLs are bundled with the build. Finishing setup only saves
configuration; it does not start a stream.
The Discord identity token is stored separately from `config.json` and
protected with Windows DPAPI for the current Windows user, so it survives app
restarts without exposing the token as plaintext. Disconnecting Discord removes
the protected local token. Startup is controlled through the current user's
Windows Run registry key.

The lobby is public by design. While live, the Worker publishes a distinct
viewer-only token and the stream's ICE configuration; it never publishes the
host token. Anyone with the lobby URL can see listed streams and watch them.
TURN credentials in the host's ICE configuration are therefore visible to
lobby visitors; use short-lived credentials if your relay provider supports
them.

For the manual SDP test tool, run `go run ./cmd/streamy-manual`. It prints an
offer to paste into the local test viewer and waits for the browser's answer.

### Run the local Activity stack (relay, gateway, viewer)

The optional Activity path replaces WebRTC with one WebSocket media relay. Three
services run from `streamy activity`, all on this machine by default:

| Service | Default address | Purpose |
| --- | --- | --- |
| relay | `:8081` | `/v1/publish/<room>` accepts the desktop app's H.264/Opus frames; `/v1/watch/<room>` fans them out |
| bot | `:8083` | lobby API, Discord OAuth, tunnel registration |
| gateway | `:8080` | serves the viewer and proxies `/api/*` and `/.proxy/relay/*` on one port |

```powershell
go run ./cmd/streamy activity                    # relay + bot + gateway
go run ./cmd/streamy activity -publish-token dev-token
go run ./cmd/streamy activity -gateway :9000 -relay :9001 -bot :9002
```

Discord OAuth is optional: without `DISCORD_CLIENT_ID`,
`DISCORD_CLIENT_SECRET` and `DISCORD_IDENTITY_SECRET` the relay, lobby and
viewer still work, and only `/api/auth/*` answers 503. The viewer is embedded
at `http://127.0.0.1:8080/`; use `-viewer-dir` to serve a different build. It
decodes the relay's H.264 with WebCodecs, so use Chrome or Edge. Audio packets
are counted but not played yet.

Point the desktop app at the stack with a relay-only config block (the room is
the Discord channel ID that viewers pass as `?room=`; the token must match
`-publish-token`):

```json
{
  "signaling": { "viewerURL": "" },
  "stream": {
    "relay": {
      "enabled": true,
      "only": true,
      "url": "ws://127.0.0.1:8080/.proxy/relay",
      "token": "dev-token",
      "activityRoom": "123456789012345678"
    }
  }
}
```

Then open `http://127.0.0.1:8080/?room=123456789012345678`, press the hotkey,
and the relay's frames should appear in the page. A direct relay URL
(`ws://127.0.0.1:8081`) also works if you do not need the viewer proxy.

When `stream.relay.enabled` is true and the relay URL points at this machine,
the tray starts `streamy activity` for you (and stops it on quit), so there is
no second command to run. It also adopts an already-running healthy stack
instead of starting a second copy. The listen ports follow the URL:
`ws://127.0.0.1:9000/.proxy/relay` starts the gateway on 9000, the relay on 8081
and the bot on 8083; a direct `ws://127.0.0.1:9001` starts the relay on 9001 and
the gateway on 8080. If a port is busy, the child logs the exact failure to
`%APPDATA%\streamy\stack\activity.log` and the app keeps running. After editing
the relay block, restart Streamy so the launcher picks it up.

### Configure the Discord bot (optional)

The bot posts one plain message containing the public viewer link when a stream
starts and deletes it when the stream stops. It is built into `streamy.exe` as
the `bot` subcommand and runs on the same machine. It has no Activity, button,
slash command, relay or tunnel. Create `stack\bot\.env` (installed) or
`../stream-site/bot/.env` (checkout) with `DISCORD_TOKEN` and
`STREAMY_API_KEY`, then configure:

```json
{
  "stream": {
    "maxViewers": 10,
    "stunServers": ["stun:stun.l.google.com:19302"],
    "turnServers": null,
    "discord": {
      "channelID": "123456789012345678",
      "notifyURL": "http://127.0.0.1:8791/api/notify",
      "notifyKey": "<STREAMY_API_KEY>"
    },
    "stack": {
      "autoStart": true,
      "script": ""
    }
  }
}
```

Notes:

- `channelID` is the Discord text channel that receives the link. The bot falls
  back to `WATCH_CHANNEL_ID` from its `.env` when the app omits it.
- `notifyURL` and `notifyKey` are optional. A failed notification is logged and
  does not stop the stream. The bot posts only the viewer link: there is no
  Watch button and no Activity to launch.
- `stack.autoStart` lets the app start and stop the built-in `streamy bot`
  subcommand. An empty `stack.script` means "use the built-in bot"; a non-empty
  path is a legacy override for a custom launcher.
- For a local test, run `go run ./cmd/streamy bot -env ..\stream-site\bot\.env`
  from the repository root, then
  `../stream-site/dev/configure-desktop.ps1 -ChannelID <id>` to write the
  `discord` block. The bot listens on `http://127.0.0.1:8791` by default.
- The bot is optional. Without it, the app still creates the viewer link and
  copies it to the clipboard. See `../stream-site/bot/README.md` for the HTTP
  API and environment variables.

### Go concepts used in phase 3

- `io.Reader` and `io.Writer` connect ffmpeg's stdout to the video parser without
  saving a second copy of the stream to disk.
- A goroutine reads the encoded bytes while the main command waits for the
  user's answer, cancellation, or a WebRTC failure. Channels report those
  events back to the main lifecycle.
- `context.Context` carries cancellation through capture startup and shutdown;
  ffmpeg is stopped through the existing graceful runner rather than abandoned.
- The `rtc.Sender` owns a Pion peer connection, while `signal.Signaler` isolates
  the room transport. This keeps capture, WebRTC and signaling independently
  testable.

For a build with **no console window** (what you want for daily use):

```powershell
go build -ldflags "-H=windowsgui" -o streamy.exe ./cmd/streamy
```

Flags:

| Flag | Meaning |
| --- | --- |
| `-config <path>` | Use a different config file |
| `-headless` | Run without a tray icon |
| `-record-only` | Record an MP4 instead of streaming |
| `-log-level <level>` | Override `logLevel` from the config (`debug`, `info`, `warn`, `error`) |
| `-version` | Print the version and exit |

## Where things live

```
%AppData%\streamy\config.json    settings (hand editable)
%AppData%\streamy\streamy.log    append-only log
```

`streamy.log` is the first place to look when something behaves oddly — every
decision the app makes is logged there.

## Record-only mode

With `-record-only`, the hotkey writes an H.264 MP4 to:

```
%UserProfile%\Videos\Streamy\streamy-2026-01-02_15-04-05.mp4
```

Recordings go in **Videos**, not next to the config file, because they are yours
and not the app's. The name is a local timestamp so a folder of them sorts in
order. `-recorder` in the debug CLI chooses this directory too.

Stopping is a `q` on ffmpeg's stdin rather than a kill, because that is what
makes ffmpeg write the MP4 index and leave a file that plays. Hitting Stop a
second time is safe; so is quitting the app mid-recording.

The tray uses the pipeline's own session label: **Recording** in this mode and
**Sharing** in normal live-stream mode.

## How the app picks what to capture

Pressing the hotkey runs one policy, in order:

1. **A fullscreen window** — a game, a video player, a presentation → capture the
   **monitor that window is on**, not the window itself.
2. **Otherwise** → `capture.defaultTarget`: `primary`, `mouse`, `focused`, or
   `monitor` (with `capture.monitorIndex`).

A window picked explicitly (the picker, or `-window` in the debug CLI) is
recorded as the rectangle it occupies on the composited desktop. `ddagrab`
duplicates that monitor and crops the rectangle out of it; GDI copies the same
region when duplication is not available.

The choice and the reason are logged every single time:

```
capture target chosen target="monitor 2 (1920x1080+0+0, dxgi output 0)"
  reason="capture.defaultTarget is \"primary\" (and no fullscreen window was found)"
  grabber=ddagrab encoder=h264_nvenc
```

Three parts of that are worth knowing about.

**A window is never captured by title.** gdigrab's `title=` selector draws the
window into its own surface, which the GPU does not composite, so a
GPU-composited window — Chrome, Discord, the Electron apps, most games — records
a black frame. A window is recorded as the rectangle it occupies on screen
instead, which is exactly the pixels the user sees. The trade-off is that a
window covered by another window shows the covering window, not its own hidden
pixels.

**"Fullscreen" means fullscreen.** A maximised window stops at the taskbar, and
recording a window that merely fills most of a screen is not what anyone means by
fullscreen capture, so it is not treated as one. The tray, the desktop (Progman),
Explorer's shell windows, and the app's own windows are never capturable.

**Which screen is screen 0?** Windows, ffmpeg and the graphics driver each number
the screens differently, and only one of those numbers is the one ffmpeg's
`ddagrab` filter wants: the DXGI output index. Getting this wrong records the
wrong monitor, so it is not guessed — the monitor and the output are matched
through the `HMONITOR` handle that both APIs report, and that handle identifies
the screen exactly. `capture/win_dxgi.go` explains why the obvious alternative
(the `id` field in the Windows display configuration API) does not work: it is a
driver-assigned number — 33024, 33025, 33029 on the machine this was built on —
and has nothing to do with an output index. Run `streamy-capture -list-monitors`
to see the mapping.

## The debugging CLI: `streamy-capture`

`cmd/streamy-capture` drives the capture and encode packages directly, with a
flag for every knob the app decides for you. Nothing about it is needed to run
the app; it exists because "the video is wrong" is not a useful bug report.

```powershell
go run ./cmd/streamy-capture -list-monitors            # monitors, each with its DXGI index
go run ./cmd/streamy-capture -list-windows             # what a picker would offer
go run ./cmd/streamy-capture -list-paths               # raw display paths (diagnostics)
go run ./cmd/streamy-capture -encoders                 # probe every H.264 encoder
go run ./cmd/streamy-capture -print-command            # print the ffmpeg command and stop
go run ./cmd/streamy-capture -duration 10s -o out.mp4
go run ./cmd/streamy-capture -duration 10s -encoder x264 -fps 15
go run ./cmd/streamy-capture -monitor 1 -duration 5s   # a particular screen
go run ./cmd/streamy-capture -window "Firefox" -duration 5s
go run ./cmd/streamy-capture -recorder -duration 5s    # the exact path the tray app uses
```

`-print-command` is the quickest answer to "what is ffmpeg actually being asked
to do", and what it prints can be pasted into a terminal unchanged.

| Flag | Meaning |
| --- | --- |
| `-duration <d>` | Stop after `d` (default `5s`; `0` means until Ctrl+C) |
| `-o <path>` | Where to write (default: a temporary file) |
| `-stdout` | Write the encoded stream to stdout instead of a file |
| `-format <f>` | `mp4` (default), `mkv`, `mp4-frag`, `h264` |
| `-encoder <e>` | `auto` (default), `nvenc`, `qsv`, `amf`, `x264` (ffmpeg's `libx264`, software) |
| `-grabber <g>` | `auto` (default), `ddagrab`, `gdigrab` |
| `-monitor <n>` | Capture monitor `n` instead of auto-selecting |
| `-window <text>` | Capture the window whose title contains `text` (recorded as its on-screen rectangle) |
| `-target` | Print what would be captured and why, then exit |
| `-recorder` | Go through `pipeline.Recorder`, the path the tray app uses |
| `-fps`, `-bitrate`, `-scale` | Override the encoding settings |
| `-mouse` | Draw the pointer into the capture (default true) |
| `-titles` | Print window titles in `-list-windows` (off by default) |
| `-v` | Show ffmpeg's own diagnostics |

With `-stdout` the encoded video owns stdout and every progress line moves to
stderr, so the two can be piped separately:

```powershell
cmd /c "streamy-capture.exe -stdout -duration 5s -format h264 > stream.h264 2> log.txt"
```

`-list-windows` prints window titles only when `-titles` is given. Titles are
whatever you happen to have open, and this command's output tends to end up
pasted into a bug report; the default output is process ids, window classes and
rectangles, which is enough to diagnose a capture problem.

## Settings

Everything is JSON with sensible defaults; the file is created on first run. The
`capture` and `encoding` blocks are read by phase 2; `stream` and `signaling`
are read from phase 3 onwards.

```json
{
  "schema": 3,
  "setupComplete": false,
  "runAtStartup": false,
  "logLevel": "info",
  "hotkey": {
    "enabled": true,
    "toggle": "Ctrl+Shift+F9",
    "picker": "Ctrl+Shift+F10"
  },
  "capture": {
    "defaultTarget": "primary",
    "monitorIndex": 0,
    "alwaysShowPicker": false,
    "autoSwitchTarget": false,
    "captureCursor": true
  },
  "indicator": {
    "enabled": true,
    "position": "top-right"
  },
  "encoding": {
    "encoder": "auto",
    "ffmpegPath": "",
    "resolution": "source",
    "fps": 30,
    "bitrateKbps": 6000,
    "keyframeSeconds": 1,
    "encoderThreads": 0,
    "extraEncoderArgs": null
  },
  "stream": {
    "maxViewers": 10,
    "stunServers": ["stun:stun.l.google.com:19302"],
    "turnServers": null,
    "discord": {
      "channelID": "",
      "notifyURL": "",
      "notifyKey": ""
    },
    "stack": {
      "autoStart": true,
      "script": ""
    }
  },
  "signaling": { "url": "", "viewerURL": "" }
}
```

Notes:

- **Hotkeys** need at least one modifier plus one key, e.g. `Ctrl+Shift+F9`,
  `Win+Alt+S`, `Ctrl+Space`. Spelling and order are free: `control + shift + f9`
  works and is normalised to `Ctrl+Shift+F9`. Run keys `F1`–`F20`, letters,
  digits, arrows, `Space`, `Enter`, `Esc`, `Tab`, `Delete` and the media keys
  are all understood.
- **A bad value is rejected with a message** instead of failing halfway through
  a stream. `Config.Validate()` also caps viewers at the Worker limit and checks
  that STUN/TURN URLs use the right schemes.
- **The live indicator** is enabled by default in the top right corner. Valid
  positions are `top-right`, `top-left`, `bottom-right` and `bottom-left`; it is
  shown only while a stream is live.
- **Viewer count** can be `0` (use the Worker maximum) or `1`–`20`. The Worker
  still enforces its own hard cap.
- **TURN settings** are shared in the viewer link fragment so the browser can
  use the same relay. Anyone holding that link can use its TURN credentials;
  prefer short-lived relay credentials when available.
- **The Discord bot is optional.** `stream.discord.notifyURL` requires
  `notifyKey`, and `channelID` must be a Discord snowflake. A config written by
  an older version with `stream.relay` is migrated automatically; only the
  `channelID`, `notifyURL`, and `notifyKey` fields are kept.
- **A UTF-8 byte order mark is tolerated.** PowerShell's `Set-Content
  -Encoding utf8` and several editors add one, and JSON itself does not allow
  one, so without this a perfectly good config file would be silently replaced
  by the defaults. The app never writes one itself.
- Old config files keep working: a key that is missing falls back to its default
  (see `internal/config/config.go` for the trick).

## Testing

```powershell
go test ./...          # unit tests
go test -v ./internal/hotkey/            # just the hotkey parser
go test -v ./internal/capture/           # the target policy and the DXGI join
go test -v ./internal/encode/            # ffmpeg arguments
go test -v ./internal/pipeline/          # the app-facing pipeline
go test -v ./internal/rtc/               # H.264 framing and Pion negotiation
```

The clipboard is a shared, briefly-locked OS resource, so `internal/app` takes a
`copyToClipboard` function that tests replace — that way running the tests never
overwrites your real clipboard.

The capture tests are written to run **without a screen**. Anything that would
need a real monitor, a real GPU or a real ffmpeg process is kept out of the
testable function: `capture.Decide` is a pure policy over a struct,
`capture.joinMonitors` takes the monitor list and the graphics outputs as
arguments, and `encode.Args` only builds a slice of strings. The one thing that
cannot be faked is the native struct layout, which is why there is a test that
asserts byte offsets — a wrong offset there does not fail a test, it writes into
the wrong memory.

The race detector (`go test -race`) needs cgo, and therefore a C compiler, which
is not installed here (`CGO_ENABLED=0`). To enable it:

```powershell
winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e   # MinGW-w64, provides gcc
$env:Path += ";C:\Program Files\WinLibs\...\bin"          # wherever it landed
$env:CGO_ENABLED=1; go test -race ./...
```

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| "Hotkey unavailable" notification | Another app already owns that combination. `RegisterHotKey` is first-come-first-served, so change the shortcut in Settings or the config. The tray menu still works. |
| No tray icon at all | Windows 11 hides new icons by default: click the `^` chevron in the taskbar and drag Streamy onto the bar. Check `streamy.log` for a startup error. |
| Second launch exits immediately | By design — one instance only, so the hotkey and the icon are not registered twice. Look for the existing icon. |
| Pressing the hotkey does nothing | It should pop a notification. If not, run with `-log-level debug` and watch `streamy.log`. Note the shortcut fires on **key release**, not key press. |
| "Could not copy link" | Another app held the clipboard lock; the app still starts sharing. Press "Copy viewer link" again from the tray menu. |
| Antivirus flags the exe | Unsigned Go binaries are frequently flagged when built with `-H=windowsgui`. Build without that flag, or add an exclusion. |
| Log file grows forever | Logs are currently append-only; rotate `%AppData%\\streamy\\streamy.log` manually if it becomes large. |
| The recording is a black screen | `gdigrab` cannot see exclusive-fullscreen games. Grep the log for "Desktop Duplication (ddagrab) is unavailable": if it is there, Desktop Duplication is switched off — a remote-desktop or screen-locking tool can do that, and it comes back when that tool is closed. |
| The wrong screen was recorded | Read the `capture target chosen` line in the log; it names the monitor and says why. `streamy-capture -list-monitors` shows the monitor → DXGI index mapping. To pin it, set `capture.defaultTarget` to `monitor` and `capture.monitorIndex`. |
| The video is sideways | A rotated (portrait) display. The app rotates the capture from Desktop Duplication itself and leaves GDI alone, because GDI already receives a rotated desktop. `streamy-capture -list-monitors` prints the rotation it detected. |
| "ffmpeg not found" | Install it (`winget install Gyan.FFmpeg`) or put the full path in `encoding.ffmpegPath`. Leaving that empty is fine as long as ffmpeg is on `PATH`. |
| `h264_nvenc` (or `qsv`/`amf`) fails | Encoder probing tries candidates in order and reports what worked. Run `streamy-capture -encoders` to see which encoders this machine accepts, then pin one with `encoding.encoder` (`x264` always works and costs CPU). |
| WebRTC stays at “Connecting” or fails | Allow the app and browser through the firewall for UDP. STUN cannot traverse every symmetric NAT; configure a TURN server under `stream.turnServers` if needed. TURN settings are shared in the viewer link fragment. Check `streamy.log`, then try another network. |
| Bot posts no link | Check `stream.discord.notifyURL`, `notifyKey`, and `channelID`; make sure the bot is running (see `stack\.run\bot.log`) and can view/send in that channel. A failed notify is logged by the app but does not stop sharing. |
| Bot does not start | Run `streamy.exe bot -log "%TEMP%\streamy-bot.log"` by hand and read the log. The bot needs `DISCORD_TOKEN` and `STREAMY_API_KEY` in `stack\bot\.env` (or `../stream-site/bot/.env`). |
| Old live link stays in Discord | Stop the stream again, or call the bot's `/api/notify` with `"action":"stop"`. On startup the bot also scans recent channel messages for a Streamy link. |
| Bot link points at the wrong viewer | The app sends the public viewer link in the notify payload; check `signaling.viewerURL` and the `VIEWER_URL` fallback in `bot/.env`. |
| Browser shows black video or no video | Use a current browser with H.264 WebRTC support, check `streamy.log` for ffmpeg errors, and keep the viewer page open while capture starts. Exclusive fullscreen capture needs `ddagrab`; remote desktop sessions can disable it. |
| Desktop audio is missing or the stream will not start | Confirm `capture.desktopAudio` is enabled and that the selected FFmpeg build lists `libopus` (`ffmpeg -encoders`). WASAPI is provided by Windows and is not an FFmpeg input. Process-loopback exclusion requires Windows 10 build 20348 or later. Check `streamy.log` for a WASAPI activation error. |
| Viewers cannot hear audio | The viewer begins muted to allow autoplay. Use its audio button and raise the volume; mobile browsers require a user tap before playing sound. |
| Manual test refuses clipboard | Copy the answer manually from the answer box. The viewer runs on `localhost`, which normally grants clipboard permission, but browser settings can still block it. |
| Recording stops by itself after ~5 seconds | You are running `streamy-capture`, whose default `-duration` is `5s`. The tray app records until you press the hotkey again. |
| The file is huge / the machine stutters | Lower `encoding.bitrateKbps`, `encoding.fps`, or set `encoding.resolution` (e.g. `1280x720`). Software encoding at 1080p60 is expensive; a hardware encoder is the real fix. |
| The MP4 will not open, or is 0 bytes | Something killed ffmpeg instead of asking it to stop. The app sends `q` on stdin and waits, and only kills as a last resort, so in the worst case the last second is lost. |
| My settings are being ignored | The log says `could not read the config, using the defaults`, which means the file did not parse (and the reason is on that line). A UTF-8 byte order mark is already tolerated; a trailing comma is not. |
| `-stdout` produces a "corrupt" file | The text progress lines went into the byte stream. They should not: with `-stdout` every message moves to stderr. Redirect the two separately (see the `cmd /c` line above) — PowerShell 5.1's pipeline decodes bytes as text and will corrupt a binary stream. |

Windows-specific note: allow the app and browser through the firewall for UDP.
TURN may be required behind symmetric NAT; Cloudflare does not provide a free
TURN relay.

## Roadmap

| Phase | Contents | State |
| --- | --- | --- |
| 1 | Tray, hotkey, config, logging, clean shutdown | **done** |
| 2 | ffmpeg capture → file, encoder auto-detection, target policy | **done** |
| 3 | Pion WebRTC to a test viewer page with manual SDP | **done** |
| 4 | Cloudflare Worker + Durable Object signaling | **done** |
| 5 | Multi-viewer, keyframe handling, viewer limits | **done** |
| 6 | Wails + Svelte picker and settings, launched from the tray | **done** |
| 7 | Reconnect logic, README polish, build script | **done** |

The recorder can write a file or hand encoded bytes to an `io.Writer`
(`streamy-capture -stdout` exercises that path). The live pipeline reads that
same H.264 stream, broadcasts it through one shared Pion track, and opens one
PeerConnection per viewer. Signaling reconnects with bounded exponential
backoff; when it returns, peers renegotiate and wait for the next IDR frame.

### Go concepts in the completed phases

- Interfaces (`pipeline.Streamer`, `signal.Signaler`, and the capture resolver)
  let the tray, transport, and platform-specific capture code evolve separately.
- `context.Context` cancels the ffmpeg process, HTTP IPC handlers, WebSocket
  reconnect loop, and Wails child operations during shutdown.
- The tray/UI IPC combines a loopback listener with a cryptographically random
  bearer token. The Wails process is a separate executable, so its WebView
  memory is released when the window closes.
- A shared `TrackLocalStaticSample` fans encoded H.264 packets out to each
  viewer; RTCP PLI/FIR requests gate delta frames until an IDR arrives.

## Platform notes

Windows is the only target so far. Every platform-specific piece is isolated
behind an interface or a build tag so that macOS and Linux can be added without
touching the rest:

- `internal/clipboard/clipboard_windows.go` + `_other.go`
- `internal/singleinstance/singleinstance_windows.go` + `_other.go`
- `internal/notify/notify_windows.go` + `_other.go`
- `internal/tray/icon.go` picks `.ico` or `.png` by `runtime.GOOS`
- `internal/hotkey` wraps `golang.design/x/hotkey`, which is itself cross-platform
- `internal/capture/win_api.go`, `win_display.go`, `win_window.go`, `win_dxgi.go`
  are the Windows half of capture, with `resolver_windows.go` / `resolver_other.go`
  choosing between them at build time. The policy (`target.go`) and the ffmpeg
  arguments (`ffmpeg.go`) underneath are portable and know nothing about Win32.

Known gaps on non-Windows builds today: the clipboard and balloon notifications
are Windows-only, capture is Windows-only (`-list-monitors` reports that plainly
rather than failing oddly), and `fyne.io/systray` needs GTK headers (cgo) on
Linux.

## Dependencies

Kept deliberately small. Each one, and why:

| Module | Why |
| --- | --- |
| `fyne.io/systray` | The tray icon and menu. On Windows it is pure Go (Win32 syscalls, no cgo) and it owns the message loop for us. |
| `golang.design/x/hotkey` | Global shortcuts via `RegisterHotKey` on Windows. No keyboard hook, no polling. |
| `golang.org/x/sys` | The Win32 bindings we need directly: clipboard, single-instance mutex, tray balloons, monitor and window enumeration, display configuration. |
| `github.com/pion/webrtc/v4` | WebRTC peer connections and H.264 RTP packetization for browser streaming. |
| `github.com/gorilla/websocket` | The RFC 6455 client transport for the signaling protocol. |
| `github.com/wailsapp/wails/v2` | The separately built on-demand settings/picker window; not linked into the tray executable. |

Desktop Duplication, DXGI enumeration and the
display configuration API are all reached through `syscall.SyscallN` and the
handful of bindings in `internal/capture/win_api.go` and `win_dxgi.go`, which is
why the app still starts without any runtime it does not control besides ffmpeg.

No Electron, no bundled Chromium, no Wails/WebView loaded in the idle tray
process. The settings executable uses the OS-installed WebView2 runtime.

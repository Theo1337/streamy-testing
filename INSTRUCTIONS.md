# Streamy Discord Activity archive

This folder is a **pre-removal snapshot** of the Streamy source tree, taken on
2026-10-23, before the Discord Activity path was removed from the active app.

It exists so the Activity work can be restored later without reconstructing it
from scratch. It is deliberately stored outside the active repository, so the
Streamy app, installer and `findScript` lookup cannot pick it up by accident.

## What is here

```
stream-site/        full web-side source snapshot (no node_modules, dist,
                    .wrangler, .run, secrets or bot/.env)
screenshare-app/    full desktop-side source snapshot (no out, binaries or
                    node_modules)
installer/          pre-removal installer scripts and documentation
INSTRUCTIONS.md     project instructions at the time of removal
TODO.md             task list at the time of removal
```

The snapshot contains the complete Activity implementation that was removed:

- `stream-site/relay/` — standalone Go WebSocket relay for H.264/Opus.
- `stream-site/gateway/` — one-port server for viewer + `/api` + `/relay`.
- `stream-site/viewer/src/activity.js` — Discord Embedded App SDK bootstrap.
- `stream-site/viewer/src/transports/websocket.js`, `webcodecs.js`, `mse.js`,
  `annexb.js` — Activity relay playback (WebCodecs with MSE fallback).
- `stream-site/bot/` — the old bot with `/token`, voice-channel resolution and
  the Activity notification shape.
- `stream-site/worker/src/tunnel.js` and the tunnel routes in `src/index.js` /
  `src/lobby.js` — stable front door for the self-hosted relay.
- `stream-site/dev/start-activity.ps1` and `configure-desktop.ps1` — local
  relay/gateway/bot/tunnel launcher.
- `stream-site/DISCORD_ACTIVITY.md` — full setup, deployment and test plan.
- `screenshare-app/internal/output/websocket.go` (and `h264.go`) — relay
  publisher.
- `screenshare-app/internal/pipeline/livestream.go` — relay output and Activity
  notification branches.
- `screenshare-app/internal/config/config.go` — `stream.relay` publisher fields
  and `ActivityStack`.
- `screenshare-app/internal/stack/stack.go` — relay/gateway/bot/tunnel
  supervisor.
- `installer/` — packaging that bundled relay, gateway, viewer, cloudflared and
  the tunnel admin secret.

## What was kept in the active app

The public WebRTC path was kept untouched:

- Cloudflare Worker signaling, lobby and Discord OAuth identity.
- Public viewer lobby and WebRTC playback.
- Discord bot that posts and deletes the plain public viewer link.
- Desktop capture, encoding, WebRTC publishing, settings, picker, startup and
  target switching.

The active config now uses `stream.discord` for the bot notification settings.
An older `stream.relay` block is migrated automatically when the active app
loads an old config.

## How to restore

1. Stop Streamy and any local bot/relay/gateway processes.
2. Copy the files you need back into the active repositories. For a full
   restore, copy the contents of `stream-site/` over
   `D:\Documents\Jobs\Projetos\streamy\stream-site\` and
   `screenshare-app/` over `D:\Documents\Jobs\Projetos\streamy\screenshare-app\`,
   then copy `installer/` back over the active installer tree.
3. Recreate `stream-site\bot\.env` from `.env.example`; secrets were
   intentionally excluded from this archive.
4. Recreate the tunnel admin token and Cloudflare secrets separately. They were
   also intentionally excluded.
5. Run `npm install` in the viewer, worker and bot folders, and rebuild the
   relay/gateway/installer with the scripts in this snapshot.

## External state that is not in this archive

This snapshot is source code only. It does not include or change:

- the Cloudflare Worker deployment, Pages deployment, secrets or Durable Object
  state;
- the Discord application, Activity configuration, URL mappings or bot token;
- any VPS relay, DNS records or TLS certificates;
- `bot/.env`, tunnel admin tokens or other credentials.

Those must be handled separately if the Activity is ever restored.

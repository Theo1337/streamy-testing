<script>
  import { onMount } from "svelte";

  let cfg;
  let targets = { monitors: [], windows: [] };
  let status = { sharing: false, text: "Loading" };
  let identity = { connected: false, profile: {}, error: "" };
  let loading = true;
  let saving = false;
  let busy = false;
  let identityBusy = false;
  let previewData = "";
  let previewError = "";
  let previewBusy = false;
  let thumbnails = {};
  let thumbnailBusy = false;
  let thumbnailPending = false;
  let thumbnailGeneration = 0;
  let recordingHotkey = "";
  let hotkeyError = "";
  let error = "";
  let message = "";
  let selected = { kind: "monitor", index: 0, handle: "" };
  let setupBusy = false;

  // UI state
  let section = "general";
  let pickerTab = "displays";
  let viewMode = "settings";
  let pickingMonitorForSettings = false;
  let updateProgress = 0;
  let updateTotal = 0;
  let updateVersion = "";
  let updatePhase = "checking";
  let updateError = "";
  let updateTerminal = false;
  let updateComplete = false;
  let updatePollingInFlight = false;
  let updateProgressTimer = null;
  let updateCloseScheduled = false;
  let updatePollingActive = false;
  let checkingUpdate = false;
  let updateCheckMessage = "";
  let updateCheckError = false;

  $: updateDeterminate = updateTotal > 0 && updatePhase === "downloading";
  $: updatePercent = updateDeterminate
    ? Math.min(100, Math.max(0, (updateProgress / updateTotal) * 100))
    : 0;
  $: updatePhaseText = updatePhaseLabel(updatePhase);

  function updatePhaseLabel(phase) {
    switch (phase) {
      case "checking": return "Checking for updates…";
      case "downloading": return "Downloading update…";
      case "waiting-for-idle": return "Update downloaded — waiting for Streamy to be idle…";
      case "installing": return "Installing update silently…";
      case "restarting": return "Restarting Streamy…";
      case "complete": return "Update complete.";
      case "failed": return "Update could not be completed.";
      default: return "Preparing update…";
    }
  }

  function formatBytes(bytes) {
    if (bytes === 0 || bytes === undefined) return "0 B";
    const k = 1024;
    const sizes = ["B", "KB", "MB", "GB"];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + " " + sizes[i];
  }

  const navItems = [
    { id: "general", label: "General" },
    { id: "identity", label: "Stream identity" },
    { id: "video", label: "Video" },
  ];

  const resolutionOptions = [
    { value: "854x480", label: "480p" },
    { value: "1280x720", label: "720p" },
    { value: "1920x1080", label: "1080p" },
    { value: "source", label: "Source" },
  ];

  const fpsOptions = [15, 30, 60];

  const bridge = () => window.go?.main?.SettingsApp;

  // Display labels are deliberately independent of the device name Windows
  // reports ("\\.\DISPLAY1"): users just want Display 1, Display 2, ...
  const monitorName = (m) => `Display ${m.index + 1}`;

  // The worker reports bounds as "1920x1080+0+0" because ffmpeg needs the
  // offset. People only need the resolution in the picker, so keep the origin
  // out of the label.
  const monitorResolution = (bounds) => {
    const match = /^(\d+)x(\d+)/.exec(String(bounds || ""));
    return match ? `${match[1]}x${match[2]}` : String(bounds || "");
  };

  $: monitors = [...targets.monitors].sort((a, b) => a.index - b.index);
  $: windows = [...targets.windows].sort((a, b) =>
    (a.title || "").localeCompare(b.title || "", undefined, { sensitivity: "base" }),
  );

  const targetKey = (kind, index, handle) =>
    kind === "window" ? `window:${handle}` : `monitor:${index}`;

  async function refreshTargets() {
    targets = await bridge().GetTargets();
    if (selected.kind === "monitor" && !targets.monitors.some((monitor) => monitor.index === selected.index)) {
      selected.index = targets.monitors[0]?.index ?? 0;
    }
    thumbnailGeneration += 1;
    thumbnails = {};
    if (viewMode === "share") void loadThumbnails();
  }

  async function loadThumbnails() {
    if (thumbnailBusy) {
      thumbnailPending = true;
      return;
    }
    const api = bridge();
    if (!api) return;

    const generation = thumbnailGeneration;
    const queue = [
      ...targets.monitors.map((monitor) => ({
        key: targetKey("monitor", monitor.index, ""),
        kind: "monitor",
        index: monitor.index,
        handle: "",
      })),
      ...targets.windows.map((win) => ({
        key: targetKey("window", 0, win.handle),
        kind: "window",
        index: 0,
        handle: String(win.handle),
      })),
    ].filter((item) => !thumbnails[item.key]);

    thumbnailBusy = true;
    try {
      let next = 0;
      const worker = async () => {
        while (next < queue.length) {
          if (document.hidden) return;
          const item = queue[next++];
          try {
            const data = await api.GetPreview(item.kind, item.index, item.handle);
            if (data && generation === thumbnailGeneration) {
              thumbnails = { ...thumbnails, [item.key]: data };
            }
          } catch {
            // Some windows (DRM-protected, minimized, or elevated) cannot be captured.
          }
        }
      };
      await Promise.all(Array.from({ length: Math.min(2, queue.length) }, worker));
    } finally {
      thumbnailBusy = false;
      if (thumbnailPending) {
        thumbnailPending = false;
        void loadThumbnails();
      }
    }
  }

  async function refreshStatus() {
    status = await bridge().GetStatus();
  }

  async function refreshIdentity() {
    identity = await bridge().GetDiscordIdentity();
  }

  async function refreshPreview() {
    if (!selected.kind || previewBusy) return;
    const request = { ...selected };
    previewBusy = true;
    previewError = "";
    try {
      const data = await bridge().GetPreview(request.kind, request.index, request.handle);
      if (request.kind === selected.kind && request.index === selected.index && String(request.handle) === String(selected.handle)) {
        previewData = data;
      }
    } catch (e) {
      if (request.kind === selected.kind && request.index === selected.index && String(request.handle) === String(selected.handle)) {
        previewData = "";
        previewError = e.message || String(e);
      }
    } finally {
      previewBusy = false;
    }
  }

  async function pollUpdateProgress() {
    if (updatePollingInFlight) return;
    updatePollingInFlight = true;
    try {
      const api = bridge();
      if (!api) return;
      const progress = await api.GetUpdateProgress();
      if (!progress) return;
      updateProgress = Number(progress.progress) || 0;
      updateTotal = Number(progress.total) || 0;
      updateVersion = progress.version || "";
      updatePhase = progress.phase || (progress.installing ? "installing" : "downloading");
      updateError = progress.error || "";
      updateTerminal = Boolean(progress.terminal) || updatePhase === "failed" || updatePhase === "complete" || updatePhase === "idle";
      updateComplete = Boolean(progress.complete) || updatePhase === "complete";
      if (updatePhase === "restarting" && !updateCloseScheduled) {
        updateCloseScheduled = true;
        // The tray will close this process as soon as the helper has accepted
        // the handoff. This is only a fallback for a slow Wails shutdown.
        setTimeout(() => bridge()?.CloseWindow?.(), 1200);
      }
      // Stop polling on explicit terminal states (failed, complete, restarting handoff)
      if (updateTerminal) {
        stopUpdateProgressPolling();
      }
    } catch (e) {
      // The tray may already be shutting down after handing off the update.
      // Treat transport loss during installing/restarting/waiting-for-idle as
      // expected shutdown, not a failure. Only mark failed if we have a
      // confirmed backend failure or were already in a terminal state.
      const expectedHandoffPhases = ["installing", "restarting", "waiting-for-idle"];
      if (!expectedHandoffPhases.includes(updatePhase) && !updateTerminal) {
        updateError = e.message || String(e);
        updatePhase = "failed";
        updateTerminal = true;
        stopUpdateProgressPolling();
      }
    } finally {
      updatePollingInFlight = false;
    }
  }

  function startUpdateProgressPolling() {
    if (updateProgressTimer) clearInterval(updateProgressTimer);
    updatePollingActive = true;
    updateCloseScheduled = false;
    void pollUpdateProgress();
    // Poll even when the window is hidden: installation is automatic and a
    // hidden-document pause used to leave the view stuck forever.
    updateProgressTimer = setInterval(() => {
      if (updatePollingActive) void pollUpdateProgress();
    }, 500);
  }

  function stopUpdateProgressPolling() {
    updatePollingActive = false;
    if (updateProgressTimer) {
      clearInterval(updateProgressTimer);
      updateProgressTimer = null;
    }
  }

  onMount(() => {
    let active = true;
    // Preview capture is a full-screen GDI copy, so it is deliberately slow
    // and only runs while the picker is actually on screen. A hidden settings
    // window must not keep a core busy.
    const previewTimer = setInterval(() => {
      if (active && !document.hidden && cfg?.setupComplete && viewMode === "share" && selected.kind) void refreshPreview();
    }, 2500);
    const statusTimer = setInterval(() => {
      if (active && !document.hidden && viewMode !== "update") {
        void refreshStatus().catch((e) => error = e.message || String(e));
      }
    }, 2000);
    const onVisibility = () => {
      if (!document.hidden && active && viewMode !== "update") void refreshStatus().catch(() => {});
    };
    document.addEventListener("visibilitychange", onVisibility);

    (async () => {
      try {
        const api = bridge();
        if (!api) throw new Error("Settings bridge is unavailable. Launch this window from the Streamy tray.");
        viewMode = (await api.GetView?.()) || "settings";
        if (viewMode === "update") {
          // Update view doesn't need config; start polling immediately.
          startUpdateProgressPolling();
        } else {
          // Settings and share views require config.
          cfg = await api.GetConfig();
          const initialResults = await Promise.allSettled([refreshTargets(), refreshStatus(), refreshIdentity()]);
          const failed = initialResults.find((result) => result.status === "rejected");
          if (failed?.status === "rejected") {
            error = failed.reason?.message || String(failed.reason);
          }
          if (cfg.setupComplete && viewMode === "share") await refreshPreview();
        }
      } catch (e) {
        error = e.message || String(e);
      } finally {
        loading = false;
      }
    })();
    return () => {
      active = false;
      clearInterval(previewTimer);
      clearInterval(statusTimer);
      stopUpdateProgressPolling();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  });

  async function save() {
    recordingHotkey = "";
    saving = true;
    error = "";
    message = "";
    try {
      await bridge().SaveConfig(cfg);
      message = "Settings saved. Changes apply to the next stream.";
    } catch (e) {
      error = e.message || String(e);
    } finally {
      saving = false;
    }
  }

  async function finishSetup() {
    if (!identity.connected) {
      error = "Connect Discord before finishing setup.";
      return;
    }
    setupBusy = true;
    error = "";
    message = "";
    const setupConfig = structuredClone(cfg);
    setupConfig.setupComplete = true;
    try {
      await bridge().SaveConfig(setupConfig);
      cfg = setupConfig;
      message = "Setup complete. Streamy is ready; it did not start streaming.";
    } catch (e) {
      error = e.message || String(e);
    } finally {
      setupBusy = false;
    }
  }

  function startHotkeyRecording(field) {
    recordingHotkey = field;
    hotkeyError = "";
  }

  function cancelHotkeyRecording() {
    recordingHotkey = "";
    hotkeyError = "";
  }

  function recordHotkey(event) {
    if (!recordingHotkey) return;
    event.preventDefault();
    event.stopPropagation();

    if (event.key === "Escape") {
      cancelHotkeyRecording();
      return;
    }
    if (event.repeat || ["Control", "Shift", "Alt", "Meta", "OS"].includes(event.key)) {
      return;
    }

    const key = hotkeyKeyName(event);
    if (!key) {
      hotkeyError = "That key can't be used. Try a letter, digit, symbol, function key, or arrow.";
      return;
    }

    const mods = [];
    if (event.ctrlKey) mods.push("Ctrl");
    if (event.shiftKey) mods.push("Shift");
    if (event.altKey) mods.push("Alt");
    if (event.metaKey) mods.push("Win");
    if (mods.length === 0) {
      hotkeyError = "Hold Ctrl, Alt, Shift or Win and press another key.";
      return;
    }

    const field = recordingHotkey;
    cfg = { ...cfg, hotkey: { ...cfg.hotkey, [field]: [...mods, key].join("+") } };
    recordingHotkey = "";
    hotkeyError = "";
  }

  function hotkeyKeyName(event) {
    const code = event.code || "";
    if (/^Key[A-Z]$/.test(code)) return code.slice(3);
    if (/^Digit[0-9]$/.test(code)) return code.slice(5);
    if (/^Numpad[0-9]$/.test(code)) return code;
    if (/^F([1-9]|1[0-9]|20)$/.test(code)) return code;

    const named = {
      Space: "Space",
      Enter: "Enter",
      NumpadEnter: "Enter",
      Tab: "Tab",
      Delete: "Delete",
      Backspace: "Backspace",
      Insert: "Insert",
      Home: "Home",
      End: "End",
      PageUp: "PageUp",
      PageDown: "PageDown",
      ArrowLeft: "Left",
      ArrowRight: "Right",
      ArrowUp: "Up",
      ArrowDown: "Down",
      Backslash: "\\",
      Slash: "/",
      Period: ".",
      Comma: ",",
      Semicolon: ";",
      Quote: "'",
      BracketLeft: "[",
      BracketRight: "]",
      Minus: "-",
      Equal: "Equal", // a literal "+" would split the shortcut string
      Backquote: "`",
      NumpadMultiply: "NumpadMultiply",
      NumpadAdd: "NumpadAdd",
      NumpadSubtract: "NumpadSubtract",
      NumpadDecimal: "NumpadDecimal",
      NumpadDivide: "NumpadDivide",
    };
    if (named[code]) return named[code];

    const key = event.key || "";
    if (/^[a-zA-Z]$/.test(key)) return key.toUpperCase();
    if (/^[0-9]$/.test(key)) return key;
    if (/^F([1-9]|1[0-9]|20)$/.test(key)) return key;
    const symbols = {
      "\\": "\\", "/": "/", ".": ".", ",": ",", ";": ";", "'": "'",
      "[": "[", "]": "]", "-": "-", "=": "Equal", "`": "`",
    };
    if (symbols[key]) return symbols[key];
    return "";
  }

  const isActiveTarget = (kind, index, handle) =>
    Boolean(status.target) &&
    status.target.kind === kind &&
    (kind === "window"
      ? String(status.target.handle) === String(handle)
      : status.target.index === index);

  async function startShareFor(target) {
    if (status.sharing && isActiveTarget(target.kind, target.index, target.handle)) {
      message = "That window is already being streamed.";
      return;
    }
    busy = true;
    error = "";
    selected = { ...target };
    try {
      if (status.sharing) {
        await bridge().SwitchShare(target.kind, target.index, target.handle);
        message = "Switching the live stream to the selected target…";
      } else {
        await bridge().StartShare(target.kind, target.index, target.handle);
        message = "Stream start requested. The viewer link is copied by the tray.";
      }
      await new Promise((resolve) => setTimeout(resolve, 300));
      await refreshStatus();
      // Close the picker window when streaming starts successfully
      if (status.sharing) {
        bridge()?.CloseWindow?.();
      }
    } catch (e) {
      error = e.message || String(e);
    } finally {
      busy = false;
    }
  }

  async function applyRunAtStartup(event) {
    const enabled = event.currentTarget.checked;
    const previous = cfg.runAtStartup;
    cfg = { ...cfg, runAtStartup: enabled };
    error = "";
    message = "";
    try {
      await bridge().SetRunAtStartup(enabled);
      message = enabled
        ? "Streamy will start automatically when you sign in to Windows."
        : "Streamy will no longer start automatically.";
    } catch (e) {
      cfg = { ...cfg, runAtStartup: previous };
      event.currentTarget.checked = previous;
      error = e.message || String(e);
    }
  }

  function activateCard(event, action) {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      action();
    }
  }

  async function connectDiscord() {
    identityBusy = true;
    error = "";
    message = "";
    try {
      await bridge().StartDiscordLogin();
      message = "Finish Discord sign-in in your browser. This window will update when it completes.";
    } catch (e) {
      error = e.message || String(e);
    } finally {
      identityBusy = false;
    }
  }

  async function disconnectDiscord() {
    identityBusy = true;
    error = "";
    try {
      await bridge().DisconnectDiscord();
      await refreshIdentity();
      message = "Discord identity disconnected.";
    } catch (e) {
      error = e.message || String(e);
    } finally {
      identityBusy = false;
    }
  }

  async function checkForUpdates() {
    checkingUpdate = true;
    updateCheckMessage = "";
    updateCheckError = false;
    try {
      const result = await bridge().CheckForUpdates();
      if (result && result.started) {
        // Update check started - poll for progress
        updatePhase = result.phase || "checking";
        updateProgress = Number(result.progress) || 0;
        updateTotal = Number(result.total) || 0;
        updateVersion = result.version || "";
        updateError = result.error || "";
        updateTerminal = Boolean(result.terminal) || result.phase === "idle";
        updateComplete = Boolean(result.complete);
        await pollUpdateProgressUntilChecked();
        if (updatePhase === "idle" && !updateError) {
          updateCheckMessage = "Streamy is up to date.";
          updateCheckError = false;
        } else if (updatePhase === "failed") {
          updateCheckError = true;
          updateCheckMessage = updateError || "Update check failed.";
        } else if (updatePhase !== "complete" && updatePhase !== "restarting") {
          updateCheckMessage = "Update available. Follow the update window.";
          updateCheckError = false;
        }
      } else {
        updateCheckError = true;
        updateCheckMessage = "Update check was not started.";
      }
    } catch (e) {
      updateCheckError = true;
      updateCheckMessage = e.message || String(e);
    } finally {
      checkingUpdate = false;
    }
  }

  async function pollUpdateProgressUntilChecked() {
    // The backend starts the check asynchronously. Stop as soon as it reports a
    // phase other than "checking"; the dedicated update window follows the
    // download and install from there.
    while (updatePhase === "checking" || updatePhase === "") {
      await new Promise((r) => setTimeout(r, 1000));
      await pollUpdateProgress();
    }
  }

  function pickMonitor(monitor) {
    selected = { kind: "monitor", index: monitor.index, handle: "" };
    previewData = "";
    void refreshPreview();
    if (pickingMonitorForSettings) {
      // User picked a monitor from settings - save the index and return to settings
      cfg = { ...cfg, capture: { ...cfg.capture, monitorIndex: monitor.index, defaultTarget: "monitor" } };
      pickingMonitorForSettings = false;
      viewMode = "settings";
      void save();
    } else if (status.sharing) {
      void startShareFor({ kind: "monitor", index: monitor.index, handle: "" });
    }
  }

  function pickWindow(win) {
    selected = { kind: "window", index: 0, handle: String(win.handle) };
    previewData = "";
    void refreshPreview();
    if (status.sharing) void startShareFor({ kind: "window", index: 0, handle: String(win.handle) });
  }

  function openMonitorPicker() {
    pickingMonitorForSettings = true;
    viewMode = "share";
    pickerTab = "displays";
    void refreshTargets().catch((e) => (error = e.message || String(e)));
  }
</script>

<svelte:head>
  <title>{viewMode === "share" ? "Streamy — Share your screen" : viewMode === "update" ? "Streamy — Updating" : "Streamy settings"}</title>
</svelte:head>

<svelte:window onkeydown={recordHotkey} />

<div class="toasts">
  {#if error}
    <div class="toast error" role="alert">
      <span>{error}</span>
      <button type="button" class="toast-close" aria-label="Dismiss error" onclick={() => (error = "")}>×</button>
    </div>
  {/if}
  {#if message}
    <div class="toast success" role="status">
      <span>{message}</span>
      <button type="button" class="toast-close" aria-label="Dismiss message" onclick={() => (message = "")}>×</button>
    </div>
  {/if}
</div>

{#if loading}
  <div class="stage"><p class="muted">Loading settings and capture targets…</p></div>
{:else if viewMode === "update"}
  <main class="update-window" aria-live="polite">
    <section class="update-card">
      <img class="update-icon" src="./icon.png" alt="" />
      <h1>Updating Streamy</h1>
      <p class="update-status">{updatePhaseText}</p>
      {#if updateVersion}
        <p class="update-version">Version {updateVersion}</p>
      {/if}

      {#if updateDeterminate}
        <div class="progress-container" aria-label="Download progress">
          <div class="progress-bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={Math.round(updatePercent)}>
            <div class="progress-fill" style="width: {updatePercent}%"></div>
          </div>
          <span class="progress-text">{(updateProgress / 1024 / 1024).toFixed(1)} MB of {(updateTotal / 1024 / 1024).toFixed(1)} MB</span>
        </div>
      {:else if updatePhase === "downloading" || updatePhase === "checking" || updatePhase === "installing" || updatePhase === "waiting-for-idle"}
        <div class="progress-container" aria-label="Update progress">
          <div class="progress-bar indeterminate"><div class="progress-fill"></div></div>
          {#if updateProgress > 0}<span class="progress-text">{(updateProgress / 1024 / 1024).toFixed(1)} MB downloaded</span>{/if}
        </div>
      {/if}

      {#if updatePhase === "failed"}
        <p class="update-error" role="alert">{updateError || "The update failed. Streamy was not changed."}</p>
      {:else if updatePhase === "complete"}
        <p class="update-success">Streamy will close and restart with the new version.</p>
      {:else if updatePhase === "restarting"}
        <p class="update-note">The installer handoff was accepted. This window will close automatically.</p>
      {:else}
        <p class="update-note">No action is required. Streamy will finish when it is safe to restart.</p>
      {/if}
    </section>
  </main>
{:else if !cfg}
  <div class="stage"><p class="muted">Settings couldn't load. Open this window from the Streamy tray icon.</p></div>
{:else if !cfg.setupComplete}
  <!-- First-time setup -->
  <div class="stage">
    <section class="setup-card">
      <p class="step">Step {identity.connected ? "2" : "1"} of 2</p>
      <h1 class="center">{identity.connected ? "Set up your stream" : "Connect Discord"}</h1>

      {#if !identity.connected}
        <p class="lead">Sign in so your name and avatar can identify your live stream in the public lobby.</p>
        <div class="identity">
          <span class="avatar placeholder" aria-hidden="true">S</span>
          <div class="identity-copy">
            <strong>Discord account</strong>
            <p>Streamy saves the sign-in securely on this Windows account.</p>
          </div>
        </div>
        {#if identity.error}<p class="inline-error" role="alert">{identity.error}</p>{/if}
        <button type="button" class="btn block" disabled={identityBusy} onclick={connectDiscord}>
          {identityBusy ? "Opening…" : "Connect Discord"}
        </button>
      {:else}
        <div class="identity">
          {#if identity.profile.avatar}
            <img class="avatar" src={identity.profile.avatar} alt="" />
          {:else}
            <span class="avatar placeholder" aria-hidden="true">S</span>
          {/if}
          <div class="identity-copy">
            <strong>Connected as {identity.profile.name}</strong>
            <p>Discord name and avatar will appear in the public lobby.</p>
          </div>
        </div>

        <div class="fields single">
          <label class="field">
            <span class="field-label">Default capture target</span>
            <select bind:value={cfg.capture.defaultTarget}>
              <option value="primary">Primary display</option>
              <option value="focused">Focused window</option>
              <option value="monitor">Specific display</option>
            </select>
          </label>
          {#if cfg.capture.defaultTarget === "monitor"}
            <div class="field">
              <span class="field-label">Specific display</span>
              <button type="button" class="btn" onclick={() => openMonitorPicker()}>
                Pick display
              </button>
              {#if monitors.length > 0}
                <span class="muted" style="margin-left: 12px;">Current: {monitorName(monitors[cfg.capture.monitorIndex] || monitors[0])}</span>
              {/if}
            </div>
          {/if}
        </div>
        <div class="rows">
          <label class="toggle">
            <span class="toggle-text">Run Streamy when I sign in to Windows</span>
            <input type="checkbox" checked={cfg.runAtStartup} onchange={applyRunAtStartup} />
            <i class="track" aria-hidden="true"></i>
          </label>
        </div>
        <p class="hint">Finishing setup saves these settings only. You can start streaming later from the tray.</p>
        <div class="actions">
          <button type="button" class="btn secondary" disabled={setupBusy || identityBusy} onclick={disconnectDiscord}>Change Discord account</button>
          <button type="button" class="btn" disabled={setupBusy} onclick={finishSetup}>{setupBusy ? "Saving…" : "Finish setup"}</button>
        </div>
      {/if}
    </section>
  </div>
{:else}
  <div class="app" class:picker-only={viewMode === "share"}>
    {#if viewMode !== "share"}
      <aside class="sidebar">
        <div class="brand">
          <img class="brand-icon" src="./icon.png" alt="" />
          <span>Streamy</span>
        </div>
        <nav class="side-nav" aria-label="Streamy sections">
          <div class="side-group">Settings</div>
          {#each navItems as item (item.id)}
            <button
              type="button"
              class="nav-item"
              class:active={section === item.id}
              aria-current={section === item.id ? "page" : undefined}
              onclick={() => (section = item.id)}
            >{item.label}</button>
          {/each}
        </nav>

        <div class="user-panel">
          {#if identity.connected && identity.profile.avatar}
            <img class="avatar sm" src={identity.profile.avatar} alt="" />
          {:else}
            <span class="avatar sm placeholder" aria-hidden="true">S</span>
          {/if}
          <div class="user-copy">
            <strong>{identity.connected ? identity.profile.name : "Discord not connected"}</strong>
            <span>{status.text || (status.sharing ? "Live" : "Not sharing")}</span>
          </div>
          {#if status.sharing}<span class="live-badge">Live</span>{/if}
        </div>
      </aside>
    {/if}

    <div class="content">
      <div class="scroll">
        <div class="inner" class:picker-inner={viewMode === "share"}>
          {#if viewMode === "share"}
            <div class="page-head">
              <div class="picker-title">
                <img class="brand-icon" src="./icon.png" alt="" />
                <div>
                  <h1>Share your screen</h1>
                  <p class="muted">Hover a window or display, then press Start sharing.</p>
                </div>
              </div>
              <button type="button" class="btn secondary" onclick={() => refreshTargets().catch((e) => (error = e.message || String(e)))}>Refresh list</button>
            </div>

            <div class="tabs" role="tablist" aria-label="Capture target type">
              <button type="button" role="tab" class="tab" class:active={pickerTab === "windows"} aria-selected={pickerTab === "windows"} onclick={() => (pickerTab = "windows")}>
                <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true"><rect x="3" y="5" width="18" height="14" rx="2" fill="none" stroke="currentColor" stroke-width="2"/><path d="M3 9h18" stroke="currentColor" stroke-width="2"/></svg>
                Windows
              </button>
              <button type="button" role="tab" class="tab" class:active={pickerTab === "displays"} aria-selected={pickerTab === "displays"} onclick={() => (pickerTab = "displays")}>
                <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true"><rect x="2.5" y="4" width="19" height="12" rx="2" fill="none" stroke="currentColor" stroke-width="2"/><path d="M8 20h8M12 16v4" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>
                Displays
              </button>
            </div>

            {#if pickerTab === "windows"}
              {#if windows.length === 0}
                <p class="empty">No windows found. Open the app you want to share, then refresh the list.</p>
              {:else}
                <div class="grid" role="group" aria-label="Windows">
                  {#each windows as win (win.handle)}
                    {@const active = selected.kind === "window" && String(selected.handle) === String(win.handle)}
                    {@const live = isActiveTarget("window", 0, win.handle)}
                    <div
                      class="card"
                      class:selected={active}
                      class:live
                      role="button"
                      tabindex="0"
                      aria-pressed={active}
                      title={win.class}
                      onclick={() => pickWindow(win)}
                      onkeydown={(event) => activateCard(event, () => pickWindow(win))}
                    >
                      <span class="thumb">
                        {#if live}
                          <span class="live-pill">Live</span>
                        {/if}
                        {#if active && previewData}
                          <img src={previewData} alt="Live preview of {win.title}" />
                        {:else if thumbnails[targetKey("window", 0, win.handle)]}
                          <img src={thumbnails[targetKey("window", 0, win.handle)]} alt="Preview of {win.title}" />
                        {:else if win.thumbnail}
                          <img src={win.thumbnail} alt="Preview of {win.title}" />
                        {:else if active && previewError}
                          <span class="thumb-note">{previewError}</span>
                        {:else}
                          <svg class="thumb-empty" viewBox="0 0 24 24" width="40" height="40" aria-hidden="true"><rect x="3" y="5" width="18" height="14" rx="2" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M3 9h18" stroke="currentColor" stroke-width="1.5"/></svg>
                        {/if}
                      </span>
                      <span class="meta">
                        <span class="tile" aria-hidden="true">
                          {#if win.icon}
                            <img src={win.icon} alt="" />
                          {:else}
                            {(win.title || "?").trim().charAt(0).toUpperCase()}
                          {/if}
                        </span>
                        <span class="label"><span class="name">{win.title}</span></span>
                      </span>
                      <button
                        type="button"
                        class="start-overlay"
                        disabled={busy || live}
                        onclick={(event) => {
                          event.stopPropagation();
                          startShareFor({ kind: "window", index: 0, handle: String(win.handle) });
                        }}
                      >{live ? "Live" : busy ? "Starting…" : status.sharing ? "Switch to this window" : "Start sharing"}</button>
                    </div>
                  {/each}
                </div>
              {/if}
            {:else}
              {#if monitors.length === 0}
                <p class="empty">No displays found. Refresh the list to try again.</p>
              {:else}
                <div class="grid" role="group" aria-label="Displays">
                  {#each monitors as monitor (monitor.index)}
                    {@const active = selected.kind === "monitor" && selected.index === monitor.index}
                    {@const live = isActiveTarget("monitor", monitor.index, "")}
                    <div
                      class="card"
                      class:selected={active}
                      class:live
                      role="button"
                      tabindex="0"
                      aria-pressed={active}
                      onclick={() => pickMonitor(monitor)}
                      onkeydown={(event) => activateCard(event, () => pickMonitor(monitor))}
                    >
                      <span class="thumb">
                        {#if live}
                          <span class="live-pill">Live</span>
                        {/if}
                        {#if active && previewData}
                          <img src={previewData} alt="Live preview of {monitorName(monitor)}" />
                        {:else if thumbnails[targetKey("monitor", monitor.index, "")]}
                          <img src={thumbnails[targetKey("monitor", monitor.index, "")]} alt="Preview of {monitorName(monitor)}" />
                        {:else if monitor.thumbnail}
                          <img src={monitor.thumbnail} alt="Preview of {monitorName(monitor)}" />
                        {:else if active && previewError}
                          <span class="thumb-note">{previewError}</span>
                        {:else}
                          <svg class="thumb-empty" viewBox="0 0 24 24" width="40" height="40" aria-hidden="true"><rect x="2.5" y="4" width="19" height="12" rx="2" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M8 20h8M12 16v4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>
                        {/if}
                      </span>
                      <span class="meta">
                        <span class="tile" aria-hidden="true">{monitor.index + 1}</span>
                        <span class="label">
                          <span class="name">{monitorName(monitor)}</span>
                          <span class="sub">{monitorResolution(monitor.bounds)}{monitor.primary ? " · Primary" : ""}</span>
                        </span>
                      </span>
                      <button
                        type="button"
                        class="start-overlay"
                        disabled={busy || live}
                        onclick={(event) => {
                          event.stopPropagation();
                          startShareFor({ kind: "monitor", index: monitor.index, handle: "" });
                        }}
                      >{live ? "Live" : busy ? "Starting…" : status.sharing ? "Switch to this display" : "Start sharing"}</button>
                    </div>
                  {/each}
                </div>
              {/if}
            {/if}

          {:else}
            <form id="settings-form" onsubmit={(event) => { event.preventDefault(); save(); }}>
              {#if section === "general"}
                <h1>General</h1>
                <p class="muted">Startup, hotkeys, and what gets captured.</p>

                <h2 class="group-title">Startup and hotkeys</h2>
                <div class="rows">
                  <label class="toggle">
                    <span class="toggle-text">Run Streamy when I sign in to Windows</span>
                    <input type="checkbox" checked={cfg.runAtStartup} onchange={applyRunAtStartup} />
                    <i class="track" aria-hidden="true"></i>
                  </label>
                  <label class="toggle">
                    <span class="toggle-text">Enable global hotkey</span>
                    <input type="checkbox" bind:checked={cfg.hotkey.enabled} />
                    <i class="track" aria-hidden="true"></i>
                  </label>
                </div>
                {#if cfg.hotkey.enabled}
                <div class="fields spaced">
                  <div class="field">
                    <span class="field-label">Toggle hotkey</span>
                    <button
                      type="button"
                      class="hotkey-button"
                      class:recording={recordingHotkey === "toggle"}
                      aria-label="Record the toggle hotkey"
                      title="Click, then press the new shortcut. Esc cancels."
                      onclick={() => startHotkeyRecording("toggle")}
                      onblur={() => { if (recordingHotkey === "toggle") cancelHotkeyRecording(); }}
                    >
                      {recordingHotkey === "toggle" ? "Press keys… (Esc to cancel)" : cfg.hotkey.toggle || "Click to record"}
                    </button>
                  </div>
                  <div class="field">
                    <span class="field-label">Picker hotkey</span>
                    <button
                      type="button"
                      class="hotkey-button"
                      class:recording={recordingHotkey === "picker"}
                      aria-label="Record the picker hotkey"
                      title="Click, then press the new shortcut. Esc cancels."
                      onclick={() => startHotkeyRecording("picker")}
                      onblur={() => { if (recordingHotkey === "picker") cancelHotkeyRecording(); }}
                    >
                      {recordingHotkey === "picker" ? "Press keys… (Esc to cancel)" : cfg.hotkey.picker || "Click to record"}
                    </button>
                  </div>
                </div>
                {/if}
                {#if hotkeyError}<p class="inline-error" role="alert">{hotkeyError}</p>{/if}

                <h2 class="group-title">Updates</h2>
                <div class="rows">
                  <label class="toggle">
                    <span class="toggle-text">Disable automatic update checks<small>Required updates will still be installed automatically.</small></span>
                    <input type="checkbox" bind:checked={cfg.update.autoUpdateDisabled} />
                    <i class="track" aria-hidden="true"></i>
                  </label>
                </div>
                <button
                  type="button"
                  class="btn secondary"
                  disabled={checkingUpdate}
                  onclick={checkForUpdates}
                >
                  {#if checkingUpdate}
                    <span class="spinner"></span> Checking...
                  {:else}
                    Check for updates now
                  {/if}
                </button>
                {#if checkingUpdate}
                  <div class="update-progress" role="status" aria-live="polite">
                    <p class="update-phase">{updatePhaseText}</p>
                    <div class="progress-bar">
                      {#if updateDeterminate}
                        <div class="progress-fill" style="width: {updatePercent}%"></div>
                        <span class="progress-text">{formatBytes(updateProgress)} / {formatBytes(updateTotal)}</span>
                      {:else}
                        <div class="progress-fill indeterminate"></div>
                        <span class="progress-text">Checking for updates…</span>
                      {/if}
                    </div>
                    {#if updateVersion}<p class="update-version">Version {updateVersion}</p>{/if}
                    {#if updateError && updatePhase === "failed"}<p class="update-error">{updateError}</p>{/if}
                  </div>
                {:else if updateCheckMessage}
                  <p class="update-check-message" class:error={updateCheckError}>{updateCheckMessage}</p>
                {/if}

                <h2 class="group-title">Capture</h2>
                <div class="fields">
                  <label class="field">
                    <span class="field-label">Default target</span>
                    <select bind:value={cfg.capture.defaultTarget}>
                      <option value="primary">Primary display</option>
                      <option value="focused">Focused window</option>
                      <option value="monitor">Specific display</option>
                    </select>
                  </label>
                </div>
                {#if cfg.capture.defaultTarget === "monitor"}
                <div class="rows spaced">
                  <div class="field">
                    <span class="field-label">Specific display</span>
                    <button type="button" class="btn" onclick={() => openMonitorPicker()}>
                      Pick display
                    </button>
                    {#if monitors.length > 0}
                      <span class="muted" style="margin-left: 12px;">Current: {monitorName(monitors[cfg.capture.monitorIndex] || monitors[0])}</span>
                    {/if}
                  </div>
                </div>
                {/if}
                <div class="rows spaced">
                  <label class="toggle">
                    <span class="toggle-text">Always show picker instead of auto-selecting</span>
                    <input type="checkbox" bind:checked={cfg.capture.alwaysShowPicker} />
                    <i class="track" aria-hidden="true"></i>
                  </label>
                  <label class="toggle">
                    <span class="toggle-text">Capture system audio<small>Uses low-latency WASAPI.</small></span>
                    <input type="checkbox" bind:checked={cfg.capture.desktopAudio} />
                    <i class="track" aria-hidden="true"></i>
                  </label>
                  {#if cfg.capture.desktopAudio}
                    <label class="toggle">
                      <span class="toggle-text">Exclude Discord audio<small>Open the Discord desktop app before you start sharing.</small></span>
                      <input type="checkbox" bind:checked={cfg.capture.excludeDiscordAudio} />
                      <i class="track" aria-hidden="true"></i>
                    </label>
                  {/if}
                </div>

                <h2 class="group-title">Live indicator</h2>
                <div class="rows">
                  <label class="toggle">
                    <span class="toggle-text">Show a blinking red dot while live<small>Always above other windows and kept out of your stream.</small></span>
                    <input type="checkbox" bind:checked={cfg.indicator.enabled} />
                    <i class="track" aria-hidden="true"></i>
                  </label>
                </div>
                {#if cfg.indicator.enabled}
                  <div class="fields">
                    <label class="field">
                      <span class="field-label">Position</span>
                      <select bind:value={cfg.indicator.position}>
                        <option value="top-right">Top right</option>
                        <option value="top-left">Top left</option>
                        <option value="bottom-right">Bottom right</option>
                        <option value="bottom-left">Bottom left</option>
                      </select>
                    </label>
                  </div>
                {/if}

              {:else if section === "identity"}
                <h1>Public stream identity</h1>
                <p class="muted">Your Discord name and avatar appear beside your stream in the public lobby. Viewer names are not collected.</p>

                <div class="identity spaced">
                  {#if identity.connected && identity.profile.avatar}
                    <img class="avatar" src={identity.profile.avatar} alt="" />
                  {:else}
                    <span class="avatar placeholder" aria-hidden="true">S</span>
                  {/if}
                  <div class="identity-copy">
                    <strong>{identity.connected ? identity.profile.name : "Discord not connected"}</strong>
                    <p>{identity.connected ? "Connected" : "Connect Discord to show your name in the lobby."}</p>
                    {#if identity.error}<p class="inline-error" role="alert">{identity.error}</p>{/if}
                  </div>
                  {#if identity.connected}
                    <button class="btn secondary" type="button" disabled={identityBusy} onclick={disconnectDiscord}>Disconnect</button>
                  {/if}
                  <button class="btn" type="button" disabled={identityBusy} onclick={connectDiscord}>
                    {identityBusy ? "Opening…" : identity.connected ? "Reconnect" : "Connect Discord"}
                  </button>
                </div>

              {:else if section === "video"}
                <h1>Video</h1>
                <p class="muted">Quality and viewer limits for your stream.</p>

                <div class="fields spaced">
                  <label class="field">
                    <span class="field-label">Resolution</span>
                    <select bind:value={cfg.encoding.resolution}>
                      {#each resolutionOptions as option (option.value)}
                        <option value={option.value}>{option.label}</option>
                      {/each}
                      {#if !resolutionOptions.some((option) => option.value === cfg.encoding.resolution)}
                        <option value={cfg.encoding.resolution}>{cfg.encoding.resolution}</option>
                      {/if}
                    </select>
                  </label>
                  <label class="field">
                    <span class="field-label">Frames per second</span>
                    <select bind:value={cfg.encoding.fps}>
                      {#each fpsOptions as fps (fps)}
                        <option value={fps}>{fps}</option>
                      {/each}
                      {#if !fpsOptions.includes(cfg.encoding.fps)}
                        <option value={cfg.encoding.fps}>{cfg.encoding.fps}</option>
                      {/if}
                    </select>
                  </label>
                  <label class="field"><span class="field-label">Bitrate (kbps)</span><input type="number" min="100" step="100" bind:value={cfg.encoding.bitrateKbps} /></label>
                  <label class="field"><span class="field-label">Maximum viewers</span><input type="number" min="0" max="20" bind:value={cfg.stream.maxViewers} /></label>
                </div>

              {/if}
            </form>
          {/if}
        </div>
      </div>

      {#if viewMode !== "share"}
        <footer class="dock">
          <span class="dock-note">Streamy runs in the tray; closing this window does not stop it.</span>
          <button type="submit" form="settings-form" class="btn" disabled={saving}>{saving ? "Saving…" : "Save settings"}</button>
        </footer>
      {/if}
    </div>
  </div>
{/if}

<style>
  :global(*), :global(*::before), :global(*::after) { box-sizing: border-box; }

  :global(:root) {
    --bg-tertiary: #1e1f22;
    --bg-secondary: #2b2d31;
    --bg-primary: #313338;
    --bg-panel: #232428;
    --bg-hover: #35373c;
    --bg-active: #404249;
    --divider: rgba(78, 80, 88, 0.48);
    --text: #f2f3f5;
    --text-normal: #dbdee1;
    --text-muted: #949ba4;
    --brand: #5865f2;
    --brand-hover: #4752c4;
    --green: #23a55a;
    --green-hover: #1a6334;
    --red: #da373c;
    --red-hover: #a12828;
    --focus: #00a8fc;
  }

  :global(html), :global(body) {
    margin: 0;
    height: 100%;
    background: #313338;
    color: #dbdee1;
    overflow: hidden;
  }

  :global(body) {
    font-family: "gg sans", "Segoe UI", "Noto Sans", "Helvetica Neue", Arial, sans-serif;
    font-size: 16px;
    line-height: 1.375;
    -webkit-font-smoothing: antialiased;
  }

  :global(:focus-visible) { outline: 2px solid var(--focus); outline-offset: 2px; }

  ::-webkit-scrollbar { width: 14px; }
  ::-webkit-scrollbar-thumb { background: #1a1b1e; border: 3px solid var(--bg-primary); border-radius: 8px; }
  ::-webkit-scrollbar-track { background: transparent; }

  h1 { margin: 0 0 4px; font-size: 20px; line-height: 24px; font-weight: 700; color: var(--text); }
  h1.center { text-align: center; font-size: 24px; line-height: 30px; margin-bottom: 8px; }
  p { margin: 0; }
  .muted { color: var(--text-muted); font-size: 14px; }

  /* Buttons */
  .btn {
    height: 38px; min-width: 96px; padding: 0 16px;
    border: 0; border-radius: 3px; cursor: pointer;
    font: inherit; font-size: 14px; font-weight: 500; color: #fff;
    background: var(--brand);
    transition: background-color 0.17s ease;
  }
  .btn:hover:not(:disabled) { background: var(--brand-hover); }
  .btn.secondary { background: #4e5058; }
  .btn.secondary:hover:not(:disabled) { background: #6d6f78; }
  .btn.block { width: 100%; margin-top: 20px; height: 44px; }
  .btn:disabled { opacity: 0.5; cursor: not-allowed; }

  /* Toasts */
  .toasts {
    position: fixed; top: 12px; left: 50%; transform: translateX(-50%); z-index: 20;
    display: flex; flex-direction: column; gap: 8px;
    width: min(560px, 92vw); pointer-events: none;
  }
  .toast {
    pointer-events: auto;
    display: flex; align-items: flex-start; justify-content: space-between; gap: 12px;
    padding: 10px 14px; border-radius: 4px; border-left: 4px solid var(--red);
    background: #111214; color: var(--text); font-size: 14px;
    box-shadow: 0 8px 16px rgba(0, 0, 0, 0.24);
  }
  .toast.success { border-left-color: var(--green); }
  .toast-close { background: none; border: 0; color: var(--text-muted); font-size: 20px; line-height: 1; cursor: pointer; padding: 0 2px; }
  .toast-close:hover { color: var(--text); }

  /* Centered stages (loading, first-time setup) */
  .stage { min-height: 100vh; display: grid; place-items: center; padding: 24px; background: var(--bg-tertiary); overflow-y: auto; }
  :global(body:has(.stage)) { overflow: auto; }
  .setup-card {
    width: min(480px, 100%); padding: 32px; border-radius: 5px;
    background: var(--bg-primary); box-shadow: 0 2px 10px rgba(0, 0, 0, 0.2);
  }
  .step { text-align: center; font-size: 14px; color: var(--text-muted); margin-bottom: 4px; }
  .lead { text-align: center; color: var(--text-muted); font-size: 16px; margin-bottom: 20px; }
  .actions { display: flex; justify-content: flex-end; gap: 12px; margin-top: 24px; }

  /* App shell */
  .app { display: grid; grid-template-columns: 232px minmax(0, 1fr); height: 100vh; }
  .app.picker-only { grid-template-columns: minmax(0, 1fr); }

  .sidebar { display: flex; flex-direction: column; min-height: 0; background: var(--bg-secondary); }
  .brand { display: flex; align-items: center; gap: 10px; padding: 20px 20px 8px; font-size: 16px; font-weight: 700; color: var(--text); }
  .brand-icon { width: 24px; height: 24px; flex: none; border-radius: 5px; }
  .picker-title { display: flex; align-items: center; gap: 14px; }
  .picker-title .brand-icon { width: 38px; height: 38px; border-radius: 8px; }
  .side-nav { display: flex; flex-direction: column; gap: 2px; padding: 8px; }
  .side-group {
    padding: 16px 10px 4px; font-size: 12px; font-weight: 700;
    letter-spacing: 0.02em; text-transform: uppercase; color: var(--text-muted);
  }
  .nav-item {
    height: 34px; padding: 0 10px; border: 0; border-radius: 4px; text-align: left;
    background: none; color: var(--text-muted); font: inherit; font-size: 16px; font-weight: 500; cursor: pointer;
  }
  .nav-item:hover { background: var(--bg-hover); color: var(--text-normal); }
  .nav-item.active { background: var(--bg-active); color: #fff; }

  .user-panel {
    margin-top: auto; height: 52px; padding: 0 8px;
    display: flex; align-items: center; gap: 8px; background: var(--bg-panel);
  }
  .user-copy { display: flex; flex-direction: column; min-width: 0; flex: 1; }
  .user-copy strong { font-size: 14px; line-height: 18px; color: var(--text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .user-copy span { font-size: 12px; line-height: 13px; color: var(--text-muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .live-badge {
    flex: none; padding: 2px 6px; border-radius: 4px; background: var(--red);
    color: #fff; font-size: 11px; font-weight: 700; letter-spacing: 0.03em; text-transform: uppercase;
  }

  .avatar { width: 40px; height: 40px; border-radius: 50%; flex: none; object-fit: cover; }
  .avatar.sm { width: 32px; height: 32px; }
  .avatar.placeholder { display: grid; place-items: center; background: var(--brand); color: #fff; font-weight: 700; }

  /* Content area */
  .content { display: flex; flex-direction: column; min-width: 0; min-height: 0; background: var(--bg-primary); }
  .scroll { flex: 1; min-height: 0; overflow-y: auto; }
  .inner { max-width: 880px; padding: 40px 40px 32px; }
  .inner.picker-inner { max-width: 1180px; padding-top: 32px; margin: 0 auto; }

  .page-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; margin-bottom: 20px; }

  .group-title {
    margin: 32px 0 8px; padding-top: 24px; border-top: 1px solid var(--divider);
    font-size: 12px; font-weight: 700; letter-spacing: 0.02em; text-transform: uppercase; color: var(--text-muted);
  }
  h1 + .muted + .group-title { margin-top: 24px; }
  .spaced { margin-top: 20px; }

  /* Picker */
  .tabs { display: flex; gap: 4px; padding: 4px; margin-bottom: 20px; border-radius: 6px; background: var(--bg-tertiary); }
  .tab {
    flex: 1; height: 36px; display: flex; align-items: center; justify-content: center; gap: 8px;
    border: 0; border-radius: 4px; background: none; color: var(--text-muted);
    font: inherit; font-size: 14px; font-weight: 500; cursor: pointer;
  }
  .tab:hover { color: var(--text-normal); background: var(--bg-secondary); }
  .tab.active { background: var(--bg-active); color: #fff; }

  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(230px, 1fr)); gap: 16px; }
  .card {
    position: relative; display: flex; flex-direction: column; padding: 0; overflow: hidden;
    border: 0; border-radius: 6px; text-align: left; cursor: pointer; font: inherit;
    background: var(--bg-secondary); color: var(--text-normal);
    transition: background-color 0.12s ease, box-shadow 0.12s ease;
  }
  .card:hover, .card:focus-visible { background: var(--bg-hover); }
  .card.selected { background: var(--bg-hover); box-shadow: 0 0 0 2px var(--brand); }
  .thumb { position: relative; display: grid; place-items: center; aspect-ratio: 16 / 9; width: 100%; background: var(--bg-tertiary); overflow: hidden; }
  .thumb img { width: 100%; height: 100%; object-fit: cover; }
  .thumb-empty { color: #7c838e; }
  .thumb-note { padding: 12px; font-size: 12px; color: var(--text-muted); text-align: center; }
  .meta { display: flex; align-items: center; gap: 10px; min-width: 0; padding: 10px 12px; }
  .tile {
    flex: none; width: 24px; height: 24px; display: grid; place-items: center;
    border-radius: 6px; background: var(--bg-active); color: var(--text); font-size: 13px; font-weight: 700;
    overflow: hidden;
  }
  .tile img { width: 18px; height: 18px; object-fit: contain; }
  .card.selected .tile { background: var(--brand); }
  .start-overlay {
    position: absolute; left: 50%; top: 50%; z-index: 2;
    height: 38px; padding: 0 18px; border: 0; border-radius: 5px;
    background: var(--green); color: #fff; font: inherit; font-size: 14px; font-weight: 600;
    cursor: pointer; opacity: 0;
    transform: translate(-50%, -50%) scale(0.96);
    box-shadow: 0 6px 18px rgba(0, 0, 0, 0.5);
    transition: opacity 0.12s ease, transform 0.12s ease, background-color 0.12s ease;
    white-space: nowrap;
  }
  .start-overlay:hover:not(:disabled) { background: var(--green-hover); }
  .start-overlay:disabled { opacity: 0; cursor: default; pointer-events: none; }
  .card:hover .start-overlay:not(:disabled),
  .card:focus-within .start-overlay:not(:disabled) { opacity: 1; transform: translate(-50%, -50%) scale(1); }
  .live-pill {
    position: absolute; left: 10px; top: 10px; z-index: 2;
    padding: 3px 8px; border-radius: 4px;
    background: var(--red); color: #fff;
    font-size: 11px; font-weight: 700; letter-spacing: 0.05em; text-transform: uppercase;
    box-shadow: 0 2px 8px rgba(0, 0, 0, 0.45);
  }
  .card.live { box-shadow: 0 0 0 2px var(--red); }
  .card.live .tile { background: var(--red); }
  .label { display: block; min-width: 0; }
  .name { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 14px; font-weight: 500; color: var(--text); }
  .sub { display: block; font-size: 12px; color: var(--text-muted); }
  .empty { padding: 40px 0; text-align: center; color: var(--text-muted); font-size: 14px; }

  /* Bottom bar */
  .dock {
    flex: none; display: flex; align-items: center; gap: 16px; min-height: 68px; padding: 12px 40px;
    background: var(--bg-secondary); border-top: 1px solid var(--bg-tertiary);
  }
  .dock-note { margin-right: auto; font-size: 14px; color: var(--text-muted); }

  /* Form controls */
  .fields { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 20px 16px; }
  .fields.single { grid-template-columns: 1fr; margin-top: 20px; }
  .field { display: flex; flex-direction: column; gap: 8px; min-width: 0; }
  .field-label { font-size: 12px; font-weight: 700; letter-spacing: 0.02em; text-transform: uppercase; color: var(--text-muted); }

  input:not([type="checkbox"]), select {
    width: 100%; height: 40px; padding: 10px; border: 0; border-radius: 3px;
    background: var(--bg-tertiary); color: var(--text-normal); font: inherit; font-size: 16px;
  }

  .hotkey-button {
    width: 100%; height: 40px; padding: 0 10px;
    display: flex; align-items: center;
    border: 0; border-radius: 3px; background: var(--bg-tertiary);
    color: var(--text-normal); font: inherit; font-size: 16px; text-align: left;
    cursor: pointer; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }
  .hotkey-button:hover { background: var(--bg-hover); }
  .hotkey-button.recording {
    color: var(--focus); background: var(--bg-hover);
    box-shadow: inset 0 0 0 2px var(--focus);
  }
  input::placeholder { color: #6d6f78; }
  input:not([type="checkbox"]):focus-visible, select:focus-visible, .hotkey-button:focus-visible { outline: 2px solid var(--focus); outline-offset: 0; }
  select {
    appearance: none; padding-right: 36px; cursor: pointer;
    background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='12' height='8' viewBox='0 0 12 8'%3E%3Cpath d='M1 1l5 5 5-5' fill='none' stroke='%23dbdee1' stroke-width='2' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E");
    background-repeat: no-repeat; background-position: right 12px center;
  }
  select option { background: var(--bg-tertiary); color: var(--text-normal); }

  /* Switches */
  .rows { margin-top: 4px; }
  .rows.spaced { margin-top: 20px; }
  .toggle {
    position: relative; display: flex; align-items: center; justify-content: space-between; gap: 16px;
    padding: 12px 0; cursor: pointer;
  }
  .toggle + .toggle { border-top: 1px solid var(--divider); }
  .toggle-text { color: var(--text-normal); font-size: 16px; }
  .toggle-text small { display: block; margin-top: 2px; font-size: 14px; color: var(--text-muted); }
  .toggle input { position: absolute; opacity: 0; width: 1px; height: 1px; }
  .track {
    position: relative; flex: none; width: 40px; height: 24px; border-radius: 12px;
    background: #80848e; transition: background-color 0.17s ease;
  }
  .track::after {
    content: ""; position: absolute; top: 3px; left: 3px; width: 18px; height: 18px;
    border-radius: 50%; background: #fff; transition: transform 0.17s ease;
  }
  .toggle input:checked + .track { background: var(--green); }
  .toggle input:checked + .track::after { transform: translateX(16px); }
  .toggle input:focus-visible + .track { outline: 2px solid var(--focus); outline-offset: 2px; }

  /* Identity and info blocks */
  .identity { display: flex; align-items: center; gap: 16px; padding: 16px; border-radius: 8px; background: var(--bg-secondary); }
  .setup-card .identity { background: var(--bg-tertiary); }
  .identity-copy { flex: 1; min-width: 0; }
  .identity-copy strong { color: var(--text); font-size: 16px; }
  .identity-copy p { color: var(--text-muted); font-size: 14px; }
  .hint { font-size: 14px; color: var(--text-muted); }
  .setup-card .hint { margin-top: 12px; }
  .inline-error { margin-top: 8px; font-size: 14px; color: #fa777c; }

  /* Update check button */
  .update-check-message {
    margin-top: 12px;
    font-size: 14px;
    color: var(--green);
  }
  .update-check-message.error {
    color: #fa777c;
  }
  .spinner {
    display: inline-block;
    width: 14px;
    height: 14px;
    border: 2px solid var(--text-muted);
    border-top-color: var(--brand);
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
    margin-right: 8px;
    vertical-align: middle;
  }
  @keyframes spin {
    to { transform: rotate(360deg); }
  }

  /* Dedicated update window */
  .update-window {
    box-sizing: border-box;
    height: 100vh;
    display: grid;
    place-items: center;
    padding: 24px;
    background: var(--bg-tertiary);
    overflow: hidden;
  }
  .update-card {
    width: min(430px, 100%);
    padding: 28px 32px 24px;
    border-radius: 8px;
    background: var(--bg-primary);
    box-shadow: 0 4px 24px rgba(0, 0, 0, 0.3);
    text-align: center;
  }
  .update-icon {
    width: 52px;
    height: 52px;
    border-radius: 10px;
    margin: 0 auto 14px;
    object-fit: contain;
  }
  .update-card h1 {
    margin: 0 0 8px;
    font-size: 22px;
    font-weight: 700;
    color: var(--text);
  }
  .update-status {
    margin: 0 0 4px;
    color: var(--text-normal);
    font-size: 15px;
  }
  .update-version {
    margin: 0 0 18px;
    color: var(--text-muted);
    font-size: 13px;
  }
  .progress-container {
    display: flex;
    flex-direction: column;
    gap: 8px;
    margin-top: 18px;
  }
  .progress-bar {
    height: 8px;
    border-radius: 4px;
    background: var(--bg-tertiary);
    overflow: hidden;
  }
  .progress-fill {
    height: 100%;
    border-radius: 4px;
    background: var(--brand);
    transition: width 0.3s ease;
  }
  .progress-bar.indeterminate .progress-fill {
    width: 40%;
    animation: update-indeterminate 1.25s ease-in-out infinite;
  }
  @keyframes update-indeterminate {
    0% { transform: translateX(-125%); }
    100% { transform: translateX(275%); }
  }
  .progress-text {
    font-size: 12px;
    color: var(--text-muted);
    text-align: right;
  }
  .update-note, .update-success, .update-error {
    margin-top: 18px;
    font-size: 13px;
    color: var(--text-muted);
  }
  .update-success { color: var(--green); }
  .update-error { color: #fa777c; text-align: left; word-break: break-word; }


  @media (max-width: 760px) {
    .app { grid-template-columns: 1fr; grid-template-rows: auto minmax(0, 1fr); }
    .sidebar { flex-direction: row; overflow-x: auto; }
    .brand, .side-group, .user-panel { display: none; }
    .side-nav { flex-direction: row; padding: 8px; }
    .nav-item { white-space: nowrap; }
    .inner { padding: 24px 16px; }
    .dock { padding: 12px 16px; }
    .fields { grid-template-columns: 1fr; }
    .page-head { flex-direction: column; }
    .identity { flex-wrap: wrap; }
  }

  @media (prefers-reduced-motion: reduce) {
    .btn, .card, .track, .track::after { transition: none; }
  }
</style>
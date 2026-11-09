// Package app wires the background process together: it owns the settings, the
// global hotkey, the tray icon and the streaming pipeline, and defines the
// order everything starts and stops in.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"streamy/internal/capture"
	"streamy/internal/clipboard"
	"streamy/internal/config"
	"streamy/internal/executil"
	"streamy/internal/hotkey"
	"streamy/internal/liveindicator"
	"streamy/internal/output"
	"streamy/internal/pipeline"
	"streamy/internal/sound"
	"streamy/internal/stack"
	"streamy/internal/tray"
	"streamy/internal/update"
	"streamy/internal/version"
)

// State is the lifecycle of a sharing session. Naming the states (instead of
// using a bool) is what makes "ignore a second hotkey press while we are still
// starting" easy to express.
type State int

const (
	// StateIdle means nothing is being shared.
	StateIdle State = iota
	// StateStarting means a stream is being brought up.
	StateStarting
	// StateStreaming means viewers can watch.
	StateStreaming
	// StateStopping means the pipeline is being torn down.
	StateStopping
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "Starting\u2026"
	case StateStreaming:
		return "Sharing"
	case StateStopping:
		return "Stopping\u2026"
	default:
		return "Not sharing"
	}
}

// updatePhase is deliberately a small set of stable strings: it is exposed to
// the settings UI over IPC and is also useful in logs and support reports.
type updatePhase string

const (
	updateIdle        updatePhase = "idle"
	updateChecking    updatePhase = "checking"
	updateDownloading updatePhase = "downloading"
	updateWaiting     updatePhase = "waiting-for-idle"
	updateInstalling  updatePhase = "installing"
	updateRestarting  updatePhase = "restarting"
	updateComplete    updatePhase = "complete"
	updateFailed      updatePhase = "failed"
)

// Options configures an App.
type Options struct {
	Logger     *slog.Logger
	Config     config.Config
	ConfigPath string
	// Streamer is the capture pipeline. Nil means the stub, which refuses to
	// start so the tray, the hotkey and the shutdown path can be exercised
	// without ffmpeg.
	Streamer pipeline.Streamer
	// Headless runs without a tray icon, which is handy in tests and on CI.
	Headless bool
	// ShutdownTimeout bounds how long Stop may take. Zero means 10 seconds.
	ShutdownTimeout time.Duration
	// UpdateHTTPClient is injectable for installer download tests and deployments
	// that proxy release assets. Nil uses a five-minute download client.
	UpdateHTTPClient *http.Client
	// LaunchUpdateHelper is injectable for tests. Nil launches the bundled helper.
	LaunchUpdateHelper func(string) error
}

// App is the long lived background process.
type App struct {
	logger           *slog.Logger
	cfg              config.Config
	cfgPath          string
	streamer         pipeline.Streamer
	hotkeys          *hotkey.Manager
	pickerHotkeys    *hotkey.Manager
	indicator        liveindicator.Controller
	tray             *tray.Tray
	stack            *stack.Manager
	updateChecker    *update.Checker
	updateHTTPClient *http.Client

	// mu guards the state below. Everything that touches them goes through a
	// short critical section and then calls refresh() outside of it, because
	// refresh takes the same lock in Status().
	mu                  sync.Mutex
	state               State
	session             pipeline.Session
	ipc                 *ipcServer
	configMu            sync.Mutex
	uiProcesses         map[*exec.Cmd]chan struct{}
	activeTarget        *capture.Target
	lastInteraction     time.Time
	shuttingDown        bool
	identityToken       string
	identityProfile     discordProfile
	identityError       string
	pendingDiscordState string

	// Update tracking fields (guarded by mu). One operation owns these fields
	// from checking through restart; callers only request work through the
	// guarded entry points below.
	pendingUpdateFile    string
	pendingUpdateURL     string
	pendingUpdateVersion string
	updateProgress       int64
	updateTotal          int64
	updatePhase          updatePhase
	updateError          string
	updateRequired       bool
	updateInFlight       bool
	updateInstallClaimed bool
	updateInstalling     bool

	// copyToClipboard is a field rather than a direct call so tests can
	// substitute it: a unit test must not overwrite the real clipboard.
	copyToClipboard      func(string) error
	launchUpdateHelperFn func(string) error

	timeout  time.Duration
	quitOnce sync.Once
	quitCh   chan struct{}
}

// New builds an App. It validates the config so a typo is reported at startup
// rather than in the middle of a stream.
func New(opts Options) (*App, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	streamer := opts.Streamer
	if streamer == nil {
		streamer = pipeline.NewStub(logger)
	}
	timeout := opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	// Initialize update checker if enabled
	var updateChecker *update.Checker
	if opts.Config.Update.Enabled && strings.TrimSpace(opts.Config.Update.Repo) != "" {
		updateChecker = update.NewChecker(opts.Config.Update.Repo)
	}

	stackNotifyURL := opts.Config.Stream.Discord.NotifyURL
	stackNotifyKey := opts.Config.Stream.Discord.NotifyKey
	if strings.TrimSpace(stackNotifyURL) == "" && opts.Config.Stream.Relay.Enabled {
		stackNotifyURL = opts.Config.Stream.Relay.NotifyURL
		stackNotifyKey = opts.Config.Stream.Relay.NotifyKey
	}

	a := &App{
		logger:        logger,
		cfg:           opts.Config,
		cfgPath:       opts.ConfigPath,
		streamer:      streamer,
		hotkeys:       hotkey.NewManager(logger),
		pickerHotkeys: hotkey.NewManager(logger),
		stack: stack.New(logger, stack.Options{
			Settings:   opts.Config.Stream.Stack,
			Relay:      opts.Config.Stream.Relay,
			NotifyURL:  stackNotifyURL,
			NotifyKey:    stackNotifyKey,
			SignalingURL: opts.Config.Signaling.URL,
			LogDir:       filepath.Dir(opts.ConfigPath),
			LogLevel:   opts.Config.LogLevel,
			MaxViewers: opts.Config.Stream.MaxViewers,
			ViewerDir:  detectViewerDir(opts.ConfigPath),
		}),
		updateChecker:    updateChecker,
		updateHTTPClient: opts.UpdateHTTPClient,
		state:            StateIdle,
		updatePhase:      updateIdle,
		copyToClipboard:  clipboard.WriteText,
		timeout:          timeout,
		quitCh:           make(chan struct{}),
		uiProcesses:      make(map[*exec.Cmd]chan struct{}),
		lastInteraction:  time.Now(),
	}
	if a.updateHTTPClient == nil {
		a.updateHTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if opts.LaunchUpdateHelper != nil {
		a.launchUpdateHelperFn = opts.LaunchUpdateHelper
	} else {
		a.launchUpdateHelperFn = a.launchUpdateHelper
	}

	if !opts.Headless {
		a.tray = tray.New(tray.Options{
			Logger:     logger,
			Controller: a, // App implements tray.Controller
			Title:      "Streamy " + version.Version,
			OnReady:    a.onReady,
		})
		a.indicator = liveindicator.New(logger)
	}
	token, identityErr := loadDiscordIdentity()
	if identityErr != nil {
		a.identityError = identityErr.Error()
	} else if token != "" {
		profile, err := parseDiscordIdentity(token)
		if err != nil {
			a.identityError = "Saved Discord sign-in is invalid or expired. Connect Discord again."
		} else {
			a.identityToken = token
			a.identityProfile = profile
		}
	}
	return a, nil
}

// Config returns a copy of the settings in use.
func (a *App) Config() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg := a.cfg
	cfg.Stream.STUNServers = append([]string(nil), cfg.Stream.STUNServers...)
	cfg.Stream.TURNServers = append([]config.TURNServer(nil), cfg.Stream.TURNServers...)
	for i := range cfg.Stream.TURNServers {
		cfg.Stream.TURNServers[i].URLs = append([]string(nil), cfg.Stream.TURNServers[i].URLs...)
	}
	cfg.Encoding.ExtraEncoderArgs = append([]string(nil), cfg.Encoding.ExtraEncoderArgs...)
	return cfg
}

// Run starts everything and blocks until the app quits. Shutdown always runs,
// even when the run loop itself failed, so no ffmpeg process or hotkey can be
// left behind.
func (a *App) Run(ctx context.Context) error {
	runErr := a.runLoop(ctx)
	return errors.Join(runErr, a.shutdown(context.WithoutCancel(ctx)))
}

func (a *App) runLoop(ctx context.Context) error {
	if a.tray != nil {
		return a.tray.Run(ctx)
	}

	// Headless: nothing owns the main thread, so block on the context or quitCh.
	if err := a.onReady(); err != nil {
		a.logger.Error("startup failed", "error", err)
	}
	a.logger.Info("running headless, press Ctrl+C to quit")
	select {
	case <-ctx.Done():
	case <-a.quitCh:
	}
	return nil
}

// onReady runs once the tray icon is on screen (or immediately in headless
// mode). This is where the global hotkey is registered.
func (a *App) onReady() error {
	a.logger.Info("streamy started",
		"version", version.String(),
		"config", a.cfgPath,
		"pid", os.Getpid(),
	)
	if err := a.startIPC(); err != nil {
		a.logger.Error("settings IPC is unavailable", "error", err)
	} else {
		a.logger.Info("settings IPC ready")
	}
	if a.cfg.RunAtStartup {
		if err := setRunAtStartup(true, a.cfgPath); err != nil {
			a.logger.Error("could not update Windows startup setting", "error", err)
			a.notify("Startup setting unavailable", err.Error())
		}
	}

	go a.watchHotkeys()
	go a.watchBotIdle()

	// The local bot is only useful when it can post the public viewer link.
	if a.shouldStartBot(a.cfg) {
		a.stack.Start()
	}

	// Keep the checker alive even when updates were disabled on first launch;
	// saving settings can enable it later without requiring an app restart.
	go a.checkForUpdates()

	if err := a.registerHotkeys(a.cfg.Hotkey); err != nil {
		a.logger.Error("global hotkeys unavailable", "error", err)
		a.notify("Hotkey unavailable", fmt.Sprintf("%v\nChange the shortcuts in:\n%s", err, a.cfgPath))
	} else if !a.cfg.Hotkey.Enabled {
		a.logger.Info("global hotkeys disabled in settings")
	}

	a.refresh()
	a.notify("Streamy is running", fmt.Sprintf("Press %s to start capturing.", a.cfg.Hotkey.Toggle))
	if a.tray != nil && !a.cfg.SetupComplete {
		a.OpenSettings()
	}
	return nil
}

// watchHotkeys turns every registered shortcut press into a toggle.
func (a *App) watchHotkeys() {
	for {
		select {
		case <-a.quitCh:
			return
		case _, ok := <-a.hotkeys.Events():
			if !ok {
				return
			}
			a.ToggleSharing()
		case _, ok := <-a.pickerHotkeys.Events():
			if !ok {
				return
			}
			a.OpenPicker()
		}
	}
}

// touchInteraction postpones the ten-minute idle shutdown when the user
// explicitly opens a window, picks a target or saves settings. Background status
// polling deliberately does not call it, so a forgotten open window still lets
// the bot go quiet.
func (a *App) touchInteraction() {
	a.mu.Lock()
	a.lastInteraction = time.Now()
	a.mu.Unlock()
}

// watchBotIdle stops the local helper process after ten quiet minutes.
// Starting a stream or opening a window resets the timer, and the next stream
// starts it again lazily. This keeps a tray app that may sit unused all day
// from pinning a relay or bot process in the background.
// The Activity stack (when relay is enabled) is never stopped by the idle
// watchdog because it must stay available for Discord Activity iframe access.
func (a *App) watchBotIdle() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.quitCh:
			return
		case <-ticker.C:
		}
		a.mu.Lock()
		// Don't stop the Activity stack (relay enabled) - it must stay running
		// for Discord Activity to work at any time.
		isActivityStack := a.cfg.Stream.Relay.Enabled
		idle := !isActivityStack && a.state == StateIdle && a.stack.Running() && time.Since(a.lastInteraction) >= 10*time.Minute
		a.mu.Unlock()
		if !idle {
			continue
		}
		a.logger.Info("stopping the local stack after ten idle minutes")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := a.stack.Stop(ctx); err != nil {
			a.logger.Warn("could not stop the idle local stack", "error", err)
		}
		cancel()
	}
}

// checkForUpdates checks for updates on startup and then periodically. Optional
// automatic checks respect the user's setting; required releases still pass.
// The interval is rebuilt after every check, so a settings change takes effect
// without requiring a restart.
func (a *App) checkForUpdates() {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-a.quitCh:
			return
		case <-timer.C:
		}

		a.mu.Lock()
		enabled := a.cfg.Update.Enabled
		disabled := a.cfg.Update.AutoUpdateDisabled
		intervalDays := a.cfg.Update.CheckIntervalDays
		a.mu.Unlock()
		if enabled {
			a.performUpdateCheck(disabled)
		}

		if intervalDays <= 0 {
			intervalDays = 7
		}
		// Avoid overflowing time.Duration if a hand-edited config contains an
		// unreasonable value.
		if intervalDays > 3650 {
			intervalDays = 3650
		}
		timer.Reset(time.Duration(intervalDays) * 24 * time.Hour)
	}
}

func (a *App) beginUpdateOperation() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.updateInFlight {
		return false
	}
	a.updateInFlight = true
	a.updateInstallClaimed = false
	a.updatePhase = updateChecking
	a.updateError = ""
	a.updateRequired = false
	a.updateProgress = 0
	a.updateTotal = 0
	return true
}

func (a *App) finishUpdateIdle() {
	a.mu.Lock()
	a.updateInFlight = false
	a.updateInstallClaimed = false
	a.updateInstalling = false
	a.updateRequired = false
	a.pendingUpdateURL = ""
	a.pendingUpdateVersion = ""
	a.updateProgress = 0
	a.updateTotal = 0
	a.updatePhase = updateIdle
	a.updateError = ""
	a.mu.Unlock()
	a.refresh()
}

func (a *App) finishUpdateFailure(err error) {
	message := "update failed"
	if err != nil {
		message = err.Error()
	}
	a.mu.Lock()
	file := a.pendingUpdateFile
	a.pendingUpdateFile = ""
	a.pendingUpdateURL = ""
	a.updateInFlight = false
	a.updateInstallClaimed = false
	a.updateInstalling = false
	a.updatePhase = updateFailed
	a.updateError = message
	a.mu.Unlock()
	if file != "" {
		_ = os.Remove(file)
	}
	a.refresh()
	a.logger.Error("automatic update failed", "error", message)
	a.notify("Update failed", message)
}

// performUpdateCheck runs a single guarded check. A successful check starts the
// download itself; no tray item or UI callback is needed to advance the flow.
func (a *App) performUpdateCheck(checkRequiredOnly bool) {
	if !a.beginUpdateOperation() {
		return
	}
	a.mu.Lock()
	checker := a.updateChecker
	a.mu.Unlock()
	if checker == nil {
		a.finishUpdateIdle()
		return
	}
	a.refresh()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-a.quitCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	result, err := checker.Check(ctx)
	if err != nil {
		a.finishUpdateFailure(err)
		return
	}
	if !result.UpdateAvailable {
		a.logger.Debug("no update available", "current", result.CurrentVersion, "latest", result.LatestVersion)
		a.finishUpdateIdle()
		return
	}
	if checkRequiredOnly && !result.Required {
		a.logger.Debug("update available but auto-update disabled and not required", "latest", result.LatestVersion)
		a.finishUpdateIdle()
		return
	}
	if result.DownloadURL == "" {
		a.finishUpdateFailure(fmt.Errorf("release %q has no Windows installer", result.LatestVersion))
		return
	}

	a.mu.Lock()
	a.pendingUpdateURL = result.DownloadURL
	a.pendingUpdateVersion = result.LatestVersion
	a.updateTotal = result.DownloadSize
	a.updateRequired = result.Required
	a.updatePhase = updateDownloading
	a.mu.Unlock()
	a.refresh()
	a.openUI("update")
	a.logger.Info("update available; downloading automatically", "current", result.CurrentVersion, "latest", result.LatestVersion, "required", result.Required)
	go a.downloadAndInstallUpdate(result.DownloadURL, result.LatestVersion)
}

// downloadAndInstallUpdate downloads an installer and then either starts the
// silent handoff immediately or waits for the current stream to become idle.
func (a *App) downloadAndInstallUpdate(downloadURL, version string) {
	a.logger.Info("downloading update", "version", version, "url", downloadURL)
	tmpFile, total, err := a.downloadInstaller(downloadURL)
	if err != nil {
		a.finishUpdateFailure(err)
		return
	}

	a.mu.Lock()
	if a.shuttingDown {
		a.mu.Unlock()
		_ = os.Remove(tmpFile)
		return
	}
	a.pendingUpdateFile = tmpFile
	a.pendingUpdateVersion = version
	if total > 0 {
		a.updateTotal = total
	}
	a.updatePhase = updateWaiting
	a.mu.Unlock()
	a.refresh()
	a.notify("Update downloaded", fmt.Sprintf("Version %s will install automatically when Streamy is idle.", version))
	a.tryStartUpdateInstall()
}

// tryStartUpdateInstall is called after a download and whenever streaming stops.
// The claim under a.mu makes repeated status/UI/tray calls harmless.
func (a *App) tryStartUpdateInstall() {
	a.mu.Lock()
	if a.shuttingDown || !a.updateInFlight || a.updateInstallClaimed || a.pendingUpdateFile == "" {
		a.mu.Unlock()
		return
	}
	if a.state != StateIdle {
		a.updatePhase = updateWaiting
		a.mu.Unlock()
		a.refresh()
		return
	}
	file := a.pendingUpdateFile
	version := a.pendingUpdateVersion
	a.updateInstallClaimed = true
	a.updateInstalling = true
	a.updatePhase = updateInstalling
	a.mu.Unlock()
	a.refresh()
	go a.installPendingUpdate(file, version)
}

func (a *App) installPendingUpdate(file, version string) {
	a.mu.Lock()
	launcher := a.launchUpdateHelperFn
	a.mu.Unlock()
	if launcher == nil {
		launcher = a.launchUpdateHelper
	}
	if err := launcher(file); err != nil {
		a.finishUpdateFailure(err)
		return
	}

	a.mu.Lock()
	a.updatePhase = updateRestarting
	a.mu.Unlock()
	a.refresh()
	a.notify("Installing update", fmt.Sprintf("Version %s is being installed automatically.", version))
	// The helper waits for this process to exit before replacing files and
	// relaunching Streamy.
	a.Quit()
}

// Status implements tray.Controller.
func (a *App) Status() tray.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	var target *tray.Target
	if a.activeTarget != nil {
		target = &tray.Target{
			Kind:   a.activeTarget.Kind.String(),
			Index:  a.activeTarget.Monitor.Index,
			Handle: fmt.Sprintf("%d", a.activeTarget.Window.Handle),
		}
		if a.activeTarget.Kind == capture.KindWindow {
			target.Index = 0
		}
	}
	updateReady := a.pendingUpdateFile != "" && !a.updateInstallClaimed
	userLoggedIn := a.identityToken != ""
	return tray.Status{
		Sharing:          a.state == StateStreaming,
		ViewerURL:        a.session.ViewerURL,
		Text:             a.statusTextLocked(),
		Active:           a.session.Label,
		Target:           target,
		UserName:         a.identityProfile.Name,
		UserAvatar:       a.identityProfile.Avatar,
		UserLoggedIn:     userLoggedIn,
		UpdateReady:      updateReady,
		UpdateVersion:    a.pendingUpdateVersion,
		UpdateProgress:   a.updateProgress,
		UpdateInstalling: a.updateInstalling,
	}
}

// statusTextLocked returns the line shown at the top of the tray menu. It
// prefers the pipeline's own word for what it is doing ("Recording", later
// "Sharing") over the bare state name. The caller must hold a.mu.
func (a *App) statusTextLocked() string {
	if a.state == StateStreaming && a.session.Label != "" {
		return a.session.Label
	}
	return a.state.String()
}

// ToggleSharing starts a stream when idle and stops it when streaming. Both the
// global hotkey and the tray menu item call this.
func (a *App) ToggleSharing() {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()

	switch state {
	case StateStreaming:
		a.stopSharing()
	case StateIdle:
		a.startSharing(nil)
	default:
		// Starting or stopping: a second press this fast is almost certainly
		// the user being impatient, so ignore it instead of queueing a stop.
		a.logger.Debug("ignoring toggle while busy", "state", state.String())
	}
}

func (a *App) startSharing(target *capture.Target) {
	a.mu.Lock()
	if a.updatePhase == updateInstalling || a.updatePhase == updateRestarting {
		a.mu.Unlock()
		a.notify("Update in progress", "Streamy is restarting; please wait for the update to finish.")
		return
	}
	if a.state != StateIdle {
		a.mu.Unlock()
		return
	}
	if target == nil && a.cfg.Capture.AlwaysShowPicker {
		a.mu.Unlock()
		a.OpenPicker()
		return
	}
	a.state = StateStarting
	cfg := a.cfg
	identityToken := a.identityToken
	identityID := a.identityProfile.ID
	identityName := a.identityProfile.Name
	a.mu.Unlock()
	a.refresh()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Make sure the local bot is on its way. Waiting for it would block the
	// hotkey; the notification is best effort and the bot starts in parallel.
	if a.shouldStartBot(cfg) {
		a.stack.Start()
	}

	session, err := a.streamer.Start(ctx, pipeline.Request{
		Config: cfg, Target: target, IdentityToken: identityToken, UserID: identityID, UserName: identityName,
		OnViewerWatching: a.viewerWatching,
		OnViewerLeft:     a.viewerLeft,
	})
	if err != nil {
		a.mu.Lock()
		a.state, a.session = StateIdle, pipeline.Session{}
		a.activeTarget = nil
		a.mu.Unlock()
		a.refresh()
		a.tryStartUpdateInstall()

		if errors.Is(err, pipeline.ErrNotImplemented) {
			// No pipeline is configured: the notification is itself the test.
			// Seeing it pop up proves the hotkey, the app and the tray are all
			// wired together.
			a.logger.Info("no capture pipeline configured", "phase", 2)
			a.notify("Hotkey works", "Tray, hotkey and settings are wired up, but no pipeline is configured.")
			return
		}
		a.logger.Error("could not start the capture", "error", err)
		a.notify("Could not start capturing", err.Error())
		return
	}

	a.mu.Lock()
	a.state, a.session = StateStreaming, session
	a.activeTarget = session.Target
	a.lastInteraction = time.Now()
	a.mu.Unlock()
	a.refresh()
	a.syncLiveIndicator()
	a.updateViewerCount()

	a.logger.Info("session started",
		"label", session.Label,
		"description", session.Description,
		"viewer_origin", redactViewerURL(session.ViewerURL),
	)
	sound.PlayStart()

	// Copying the link is a nicety: if the clipboard is locked by another
	// process we still record, we just say so.
	var copyErr error
	if session.ViewerURL != "" {
		copyErr = a.copyToClipboard(session.ViewerURL)
		if copyErr != nil {
			a.logger.Warn("could not copy the link", "error", copyErr)
		}
	}
	a.announce(session, copyErr)
}

// SwitchSharing retargets a live stream when the picker selects a different
// window or display. Starting from idle is allowed too, which keeps the picker
// code to one path.
func (a *App) SwitchSharing(target capture.Target) error {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()
	if state == StateIdle {
		a.startSharing(&target)
		return nil
	}
	if state != StateStreaming {
		return errors.New("the stream is busy starting or stopping")
	}

	a.mu.Lock()
	a.state = StateStarting
	a.lastInteraction = time.Now()
	a.mu.Unlock()
	a.refresh()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := a.streamer.SwitchTarget(ctx, target)
	if err != nil {
		a.logger.Error("could not switch the stream target", "error", err)
		a.mu.Lock()
		a.state = StateStreaming
		a.mu.Unlock()
		a.refresh()
		a.syncLiveIndicator()
		var rejected *pipeline.SwitchRejectedError
		if errors.As(err, &rejected) {
			// The target was rejected before ffmpeg was restarted, so the
			// current capture is still running and viewers stay connected.
			a.notify("Could not switch the stream", "The current target is still live. "+err.Error())
			return err
		}
		a.notify("Could not switch the stream", err.Error())
		// The old capture has already stopped, so leaving the room "live"
		// would show a frozen frame forever.
		go a.stopSharing()
		return err
	}

	a.mu.Lock()
	a.state = StateStreaming
	targetCopy := target
	session.Target = &targetCopy
	a.session = session
	a.activeTarget = &targetCopy
	a.mu.Unlock()
	a.refresh()
	a.syncLiveIndicator()
	a.logger.Info("stream target switched", "target", target.Describe())
	a.notify("Stream switched", target.Describe())
	return nil
}

// shouldStartBot decides whether a local helper process should run. The
// notification bot is only useful when notifications are configured; the
// Activity stack (relay, gateway, bot) is useful whenever the relay is enabled
// locally. An explicit script is still a valid way to point at a development
// launcher.
func (a *App) shouldStartBot(cfg config.Config) bool {
	if !cfg.Stream.Stack.AutoStart {
		return false
	}
	if cfg.Stream.Relay.Enabled {
		return true
	}
	return strings.TrimSpace(cfg.Stream.Stack.Script) != "" ||
		strings.TrimSpace(cfg.Stream.Discord.NotifyURL) != ""
}

// notifyBotStopped removes the bot's live link. It is best effort: a bot outage
// must never make stopping a stream slow or fail.
func (a *App) notifyBotStopped(ctx context.Context, cfg config.Config) {
	discord := cfg.Stream.Discord
	relay := cfg.Stream.Relay
	type target struct {
		label     string
		url       string
		key       string
		channelID string
	}
	targets := []target{{
		label:     "Discord",
		url:       discord.NotifyURL,
		key:       discord.NotifyKey,
		channelID: discord.ChannelID,
	}}
	if relay.Enabled && relay.NotifyURL != "" {
		targets = append(targets, target{
			label:     "Discord Activity",
			url:       relay.NotifyURL,
			key:       relay.NotifyKey,
			channelID: relay.ChannelID,
		})
	}

	seen := make(map[string]bool, len(targets))
	for _, item := range targets {
		if strings.TrimSpace(item.url) == "" {
			continue
		}
		dedupe := item.url + "\n" + item.key + "\n" + item.channelID
		if seen[dedupe] {
			continue
		}
		seen[dedupe] = true
		notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := output.NotifyBotStop(notifyCtx, item.url, item.key, output.BotNotice{
			ChannelID: item.channelID,
		})
		cancel()
		if err != nil {
			a.logger.Warn("could not remove the Discord live link", "target", item.label, "error", err)
		}
	}
}

func redactViewerURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	if parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return parsed.Scheme + ":"
}

// announce reports a started session to the user.
//
// The wording is derived from the session rather than hard coded, because
// "sharing" and "recording to a file" are different enough that the same
// sentence would be wrong for one of them.
func (a *App) announce(session pipeline.Session, copyErr error) {
	title := sessionLabel(session, "Sharing")
	if session.LocalRecording != "" {
		title = "Recording started"
	}

	message := session.Description
	switch {
	case session.ViewerURL == "":
		// Nothing to copy; the description already says where it is going.

	case copyErr != nil:
		message += "\n\nLink (could not copy it automatically):\n" + session.ViewerURL

	case session.LocalRecording != "":
		// The link is the recording's own path, so repeating it would be
		// noise.
		message += "\n\nThe link is on your clipboard."

	default:
		message += "\n\nLink copied to your clipboard:\n" + session.ViewerURL
	}
	a.notify(title, message)
}

func sessionLabel(session pipeline.Session, fallback string) string {
	if session.Label == "" {
		return fallback
	}
	return session.Label
}

// viewerWatching plays the arrival cue for a viewer whose video connected.
// The cue runs through the default audio device, and desktop capture excludes
// Discord rather than Streamy, so viewers hear it as part of the stream too.
func (a *App) viewerWatching() {
	if !a.isStreaming() {
		return
	}
	sound.PlayViewerJoined()
	a.updateViewerCount()
}

// viewerLeft plays the departure cue for a viewer that was watching.
func (a *App) viewerLeft() {
	if !a.isStreaming() {
		return
	}
	sound.PlayViewerLeft()
	a.updateViewerCount()
}

func (a *App) updateViewerCount() {
	if a.indicator == nil {
		return
	}
	count := a.streamer.ViewerCount()
	a.indicator.SetViewerCount(count)
}

func (a *App) isStreaming() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state == StateStreaming
}

func (a *App) stopSharing() {
	a.mu.Lock()
	if a.state != StateStreaming {
		a.mu.Unlock()
		return
	}
	a.state = StateStopping
	cfg := a.cfg
	a.mu.Unlock()
	a.refresh()
	// The cue means "you are live", so drop it as soon as stopping starts
	// rather than waiting for ffmpeg to shut down.
	a.syncLiveIndicator()

	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()

	if err := a.streamer.Stop(ctx); err != nil {
		a.logger.Error("problem stopping the capture", "error", err)
		a.notify("Problem stopping", err.Error())
	}
	// Do not make the tray wait on Discord; the bot call is best effort.
	go a.notifyBotStopped(ctx, cfg)

	a.mu.Lock()
	a.state, a.session = StateIdle, pipeline.Session{}
	a.activeTarget = nil
	a.lastInteraction = time.Now()
	a.mu.Unlock()
	a.refresh()
	a.updateViewerCount() // reset to 0
	sound.PlayStop()

	// A downloaded update never interrupts a live session. The stop transition
	// is the first safe point at which the pending handoff may be claimed.
	a.tryStartUpdateInstall()
	a.logger.Info("session stopped")
}

// OpenSettings launches the settings window (sidebar, no capture picker).
func (a *App) OpenSettings() { a.openUI("settings") }

// OpenPicker launches the full-window capture picker. It deliberately has no
// sidebar, so choosing a window stays one click away.
func (a *App) OpenPicker() { a.openUI("share") }

func (a *App) openUI(view string) {
	path := a.uiExecutable()
	if path == "" {
		a.logger.Info("settings UI is not installed", "config", a.cfgPath)
		a.notify("Settings UI not found", "Build streamy-ui.exe and place it beside Streamy.\nFor now, edit:\n"+a.cfgPath)
		return
	}

	a.mu.Lock()
	ipc := a.ipc
	if a.shuttingDown || ipc == nil {
		a.mu.Unlock()
		if ipc == nil {
			a.notify("Settings unavailable", "The local settings connection is not running.")
		}
		return
	}
	a.lastInteraction = time.Now()
	a.mu.Unlock()

	cmd := exec.Command(path, "-ipc-address", ipc.address, "-ipc-token", ipc.token, "-view", view)
	// The settings UI is a Wails window. A console-subsystem build would
	// otherwise flash a terminal, but HideConsoleWindow's SW_HIDE would also
	// hide the first ShowWindow call. NoConsoleWindow suppresses only the
	// terminal.
	executil.NoConsoleWindow(cmd)
	if err := cmd.Start(); err != nil {
		a.logger.Error("could not launch the settings UI", "path", path, "error", err)
		a.notify("Could not open settings", err.Error())
		return
	}

	done := make(chan struct{})
	a.mu.Lock()
	if a.shuttingDown || a.ipc != ipc {
		a.mu.Unlock()
		_ = cmd.Process.Kill()
		close(done)
		return
	}
	a.uiProcesses[cmd] = done
	a.mu.Unlock()

	// Reap the child so it does not stay a zombie if it exits on its own.
	go func() {
		waitErr := cmd.Wait()
		a.mu.Lock()
		delete(a.uiProcesses, cmd)
		expectedShutdown := a.shuttingDown
		a.mu.Unlock()
		close(done)
		if waitErr != nil && !errors.Is(waitErr, os.ErrProcessDone) && !expectedShutdown {
			a.logger.Warn("settings UI exited with an error", "error", waitErr)
		}
	}()
	a.logger.Info("settings UI launched", "path", path, "view", view)
}

// uiExecutable returns the path of the Wails UI next to this binary, or "".
func (a *App) uiExecutable() string {
	self, err := os.Executable()
	if err != nil {
		a.logger.Warn("could not resolve tray executable path", "error", err)
		return ""
	}
	workingDir, err := os.Getwd()
	if err != nil {
		a.logger.Warn("could not resolve current working directory", "error", err)
	}
	if workingDir == "" {
		return ""
	}
	return findUIExecutable(self, workingDir)
}

func findUIExecutable(self, workingDir string) string {
	sibling := filepath.Join(filepath.Dir(self), "streamy-ui.exe")
	if isFile(sibling) {
		return sibling
	}

	// `go run` puts the tray executable in a temporary directory. In a
	// development checkout, find the UI built by build.ps1 relative to the
	// module root instead of incorrectly reporting it as missing.
	if isFile(filepath.Join(workingDir, "go.mod")) &&
		isFile(filepath.Join(workingDir, "ui", "main.go")) {
		candidate := filepath.Join(workingDir, "out", "bin", "streamy-ui.exe")
		if isFile(candidate) {
			return candidate
		}
	}
	return ""
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// CopyViewerLink implements tray.Controller.
func (a *App) CopyViewerLink() {
	a.mu.Lock()
	url := a.session.ViewerURL
	a.mu.Unlock()

	if url == "" {
		a.notify("No link yet", "Start capturing first: the link appears once a session is running.")
		return
	}
	if err := a.copyToClipboard(url); err != nil {
		a.logger.Error("could not copy the link", "error", err)
		a.notify("Could not copy link", err.Error())
		return
	}
	a.notify("Link copied", url)
}

// CheckForUpdates implements tray.Controller. The actual operation runs in one
// goroutine and is guarded by updateInFlight, so tray and IPC callers coalesce.
func (a *App) CheckForUpdates() {
	a.mu.Lock()
	configured := a.updateChecker != nil
	a.mu.Unlock()
	if !configured {
		a.notify("Update check unavailable", "Auto-update is not configured. Set a GitHub repo in settings to enable.")
		return
	}
	// An explicit user request is allowed to bypass the optional-update
	// preference; the setting only suppresses background checks.
	go a.performUpdateCheck(false)
}

// InstallPendingUpdate is retained as a compatibility action for older tray/UI
// builds. Automatic updates already call tryStartUpdateInstall themselves.
func (a *App) InstallPendingUpdate() {
	a.mu.Lock()
	ready := a.pendingUpdateFile != ""
	inFlight := a.updateInFlight
	version := a.pendingUpdateVersion
	a.mu.Unlock()
	if !ready && !inFlight {
		a.notify("No update ready", "Check for updates first.")
		return
	}
	a.logger.Info("update install requested", "version", version)
	a.openUI("update")
	a.tryStartUpdateInstall()
}

// InstallUpdate implements tray.Controller.
func (a *App) InstallUpdate() { a.InstallPendingUpdate() }

type updateProgressWriter struct {
	writer  io.Writer
	written int64
	onWrite func(int64)
}

func (w *updateProgressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.written += int64(n)
	if n > 0 && w.onWrite != nil {
		w.onWrite(w.written)
	}
	return n, err
}

// downloadInstaller downloads only a Windows executable to a unique temporary
// file. The request is cancelled when the app quits and partial files are always
// removed.
func (a *App) downloadInstaller(downloadURL string) (string, int64, error) {
	parsed, err := url.Parse(strings.TrimSpace(downloadURL))
	if err != nil || parsed.Scheme != "https" ||
		!strings.EqualFold(filepath.Ext(parsed.Path), ".exe") {
		return "", 0, fmt.Errorf("unsupported installer URL")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-a.quitCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", 0, fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "Streamy-UpdateDownloader")
	client := a.updateHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download failed: %s", resp.Status)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaErr != nil {
		mediaType = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	}
	if mediaType == "" || strings.HasPrefix(strings.ToLower(mediaType), "text/") ||
		strings.Contains(strings.ToLower(mediaType), "html") || strings.Contains(strings.ToLower(mediaType), "json") {
		return "", 0, fmt.Errorf("downloaded response is not a Windows installer (content type %q)", mediaType)
	}

	out, err := os.CreateTemp(os.TempDir(), "Streamy-Update-*.exe")
	if err != nil {
		return "", 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpFile := out.Name()
	keep := false
	defer func() {
		_ = out.Close()
		if !keep {
			_ = os.Remove(tmpFile)
		}
	}()

	a.mu.Lock()
	a.updateProgress = 0
	if resp.ContentLength > 0 {
		a.updateTotal = resp.ContentLength
	}
	a.mu.Unlock()
	lastRefresh := time.Time{}
	writer := &updateProgressWriter{writer: out, onWrite: func(n int64) {
		a.mu.Lock()
		a.updateProgress = n
		shouldRefresh := lastRefresh.IsZero() || time.Since(lastRefresh) >= 100*time.Millisecond
		if shouldRefresh {
			lastRefresh = time.Now()
		}
		a.mu.Unlock()
		if shouldRefresh {
			a.refresh()
		}
	}}
	written, err := io.Copy(writer, resp.Body)
	if err != nil {
		return "", written, fmt.Errorf("write temp file: %w", err)
	}
	if err := out.Sync(); err != nil {
		return "", written, fmt.Errorf("flush temp file: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", written, fmt.Errorf("close temp file: %w", err)
	}
	info, err := os.Stat(tmpFile)
	if err != nil {
		return "", written, fmt.Errorf("stat temp file: %w", err)
	}
	if info.Size() == 0 {
		return "", written, errors.New("downloaded installer is empty")
	}
	a.mu.Lock()
	a.updateProgress = info.Size()
	if a.updateTotal <= 0 {
		a.updateTotal = info.Size()
	}
	a.mu.Unlock()
	a.refresh()
	keep = true
	a.logger.Info("downloaded update", "size", info.Size(), "dest", tmpFile)
	return tmpFile, info.Size(), nil
}

func stageUpdateHelper(source string) (string, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open update helper: %w", err)
	}
	defer in.Close()

	out, err := os.CreateTemp(os.TempDir(), "Streamy-Updater-*.exe")
	if err != nil {
		return "", fmt.Errorf("create temporary update helper: %w", err)
	}
	path := out.Name()
	keep := false
	defer func() {
		_ = out.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return "", fmt.Errorf("copy update helper: %w", err)
	}
	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("flush update helper: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close update helper: %w", err)
	}
	keep = true
	return path, nil
}

// launchUpdateHelper hands the installer to a process that survives this app's
// shutdown. Running an installer from the live process can leave streamy.exe
// locked and has no reliable relaunch point.
func (a *App) launchUpdateHelper(installer string) error {
	if runtime.GOOS != "windows" {
		return errors.New("automatic installation is supported on Windows only")
	}
	if !isFile(installer) {
		return fmt.Errorf("installer does not exist: %s", installer)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Streamy executable: %w", err)
	}
	helper := filepath.Join(filepath.Dir(self), "streamy-updater.exe")
	if !isFile(helper) {
		if workingDir, dirErr := os.Getwd(); dirErr == nil {
			candidate := filepath.Join(workingDir, "out", "bin", "streamy-updater.exe")
			if isFile(candidate) {
				helper = candidate
			}
		}
	}
	if !isFile(helper) {
		return fmt.Errorf("update helper is not installed beside Streamy: %s", helper)
	}
	// Run a temporary copy rather than the installed helper itself. Inno Setup
	// must be able to replace streamy-updater.exe while this detached process is
	// waiting for Streamy to exit.
	runnableHelper, err := stageUpdateHelper(helper)
	if err != nil {
		return err
	}
	cmd := exec.Command(runnableHelper,
		"-installer", installer,
		"-pid", strconv.Itoa(os.Getpid()),
		"-exe", self,
		"-log", filepath.Join(os.TempDir(), "Streamy-update.log"),
	)
	executil.NoConsoleWindow(cmd)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(runnableHelper)
		return fmt.Errorf("start update helper: %w", err)
	}
	// The helper is intentionally independent. Release the process handle so
	// shutdown cannot accidentally wait for it or retain a kernel handle.
	if err := cmd.Process.Release(); err != nil {
		a.logger.Debug("could not release update helper process handle", "error", err)
	}
	return nil
}

// openInBrowser opens a URL in the default browser.
func openInBrowser(url string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	} else if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", url)
	} else {
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// detectViewerDir finds the viewer/dist directory for the Activity gateway.
// It checks multiple locations to work both in development and installed builds
// without requiring configuration.
func detectViewerDir(configPath string) string {
	// 1. Check if explicitly set in config (future: stack.viewerDir)
	// 2. Relative to executable (installed build)
	self, err := os.Executable()
	if err == nil {
		candidates := []string{
			filepath.Join(filepath.Dir(self), "..", "stream-site", "viewer", "dist"),
			filepath.Join(filepath.Dir(self), "stream-site", "viewer", "dist"),
			filepath.Join(filepath.Dir(self), "viewer", "dist"),
		}
		for _, c := range candidates {
			if info, err := os.Stat(c); err == nil && info.IsDir() {
				return c
			}
		}
	}

	// 3. Relative to working directory (go run / development)
	if wd, err := os.Getwd(); err == nil {
		candidates := []string{
			filepath.Join(wd, "..", "streamy", "stream-site", "viewer", "dist"),
			filepath.Join(wd, "stream-site", "viewer", "dist"),
			filepath.Join(wd, "viewer", "dist"),
		}
		for _, c := range candidates {
			if info, err := os.Stat(c); err == nil && info.IsDir() {
				return c
			}
		}
	}

	// 4. Old hardcoded path (config dir based)
	if configPath != "" {
		c := filepath.Join(filepath.Dir(configPath), "..", "streamy", "stream-site", "viewer", "dist")
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}

	// 5. Known development location (user's setup)
	known := `D:\Documents\Jobs\Projetos\streamy\stream-site\viewer\dist`
	if info, err := os.Stat(known); err == nil && info.IsDir() {
		return known
	}

	// 6. Empty = use embedded viewer
	return ""
}

// DownloadAndInstallUpdate is retained for programmatic callers. It performs
// the same validated download and external handoff as the automatic lifecycle.
func (a *App) DownloadAndInstallUpdate(downloadURL string) error {
	tmpFile, _, err := a.downloadInstaller(downloadURL)
	if err != nil {
		return err
	}
	if err := a.launchUpdateHelper(tmpFile); err != nil {
		_ = os.Remove(tmpFile)
		return err
	}
	return nil
}

// Quit implements tray.Controller and is the single "please stop" entry point.
func (a *App) Quit() {
	a.quitOnce.Do(func() {
		a.logger.Info("quit requested")
		close(a.quitCh)
		if a.tray != nil {
			a.tray.Quit()
		}
	})
}

// shutdown releases everything in reverse order of acquisition. Using a fresh
// context (not the cancelled one) is deliberate: cleanup needs to be able to
// wait even though the run context is already done.
func (a *App) shutdown(ctx context.Context) error {
	a.quitOnce.Do(func() { close(a.quitCh) })
	a.mu.Lock()
	a.shuttingDown = true
	uiProcesses := make([]*exec.Cmd, 0, len(a.uiProcesses))
	uiDones := make([]chan struct{}, 0, len(a.uiProcesses))
	for cmd, done := range a.uiProcesses {
		uiProcesses = append(uiProcesses, cmd)
		uiDones = append(uiDones, done)
	}
	cfg := a.cfg
	pendingFile := ""
	if a.pendingUpdateFile != "" && !a.updateInstallClaimed {
		pendingFile = a.pendingUpdateFile
		a.pendingUpdateFile = ""
	}
	a.mu.Unlock()
	if pendingFile != "" {
		_ = os.Remove(pendingFile)
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var errs []error
	for _, cmd := range uiProcesses {
		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("close settings UI: %w", err))
			}
		}
	}
	for _, done := range uiDones {
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("wait for settings UI to close: %w", ctx.Err()))
		}
	}

	if err := a.hotkeys.Unregister(); err != nil {
		errs = append(errs, err)
	}
	if err := a.pickerHotkeys.Unregister(); err != nil {
		errs = append(errs, err)
	}

	a.mu.Lock()
	wasActive := a.state == StateStreaming || a.state == StateStarting || a.state == StateStopping
	a.state, a.session = StateIdle, pipeline.Session{}
	a.mu.Unlock()

	if wasActive {
		if err := a.streamer.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop streamer: %w", err))
		}
		a.notifyBotStopped(ctx, cfg)
	}
	if a.indicator != nil {
		a.indicator.Close()
	}
	if err := a.stopIPC(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop settings IPC: %w", err))
	}
	if err := a.stack.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop local bot: %w", err))
	}

	a.logger.Info("streamy stopped")
	return errors.Join(errs...)
}

// refresh asks the tray to redraw. It is nil safe so the same code paths work in
// headless mode.
func (a *App) refresh() {
	if a.tray != nil {
		a.tray.Refresh()
	}
}

// syncLiveIndicator shows, moves or hides the blinking red dot based on the
// current state and settings. It is nil safe: headless builds have no overlay.
func (a *App) syncLiveIndicator() {
	if a.indicator == nil {
		return
	}
	a.mu.Lock()
	enabled := a.cfg.Indicator.Enabled
	position := a.cfg.Indicator.Position
	showCount := a.cfg.Indicator.ShowViewerCount
	// Record-only sessions write a file; the dot is a "you are live" cue, so
	// it stays hidden then.
	live := a.state == StateStreaming && a.session.LocalRecording == ""
	a.mu.Unlock()

	a.indicator.SetShowViewerCount(showCount)

	if live && enabled {
		a.indicator.Show(position)
		return
	}
	a.indicator.Hide()
}

// notify shows a balloon notification, or logs it when there is no tray.
func (a *App) notify(title, message string) {
	if a.tray != nil {
		a.tray.Notify(title, message)
		return
	}
	a.logger.Info("notification", "title", title, "message", message)
}

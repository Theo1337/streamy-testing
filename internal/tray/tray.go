// Package tray draws the notification area icon and its menu.
//
// Design note: the tray does not know anything about streaming. It renders a
// Status snapshot and calls back into a Controller interface, which the app
// package implements. In Go it is idiomatic for the *consumer* to declare the
// interface it needs (rather than the provider declaring "here is my API"),
// because that keeps the dependency arrow pointing one way: app -> tray, never
// tray -> app.
package tray

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"streamy/internal/notify"
)

// Status is the snapshot the menu is rendered from.
type Status struct {
	// Sharing is true while a session is live.
	Sharing bool `json:"sharing"`
	// ViewerURL is the link shown to viewers, empty when nothing is running.
	ViewerURL string `json:"viewerURL"`
	// Text is the free form line shown at the top of the menu and in the
	// tooltip, e.g. "Starting…" or "Not sharing".
	Text string `json:"text"`
	// Active is the pipeline's own word for a live session: "Recording" while
	// a file is being written, "Streaming" once phase 3 sends video to
	// viewers. The menu is built from it, so it says "Stop recording" rather
	// than "Stop sharing" when nothing is being shared. Empty means "sharing".
	Active string `json:"active"`
	// Target identifies what is being captured right now, so the picker can
	// show a Live badge on the correct card.
	Target *Target `json:"target,omitempty"`

	// User info for the tray menu
	UserName     string `json:"userName"`
	UserAvatar   string `json:"userAvatar"`   // base64 data URL or empty
	UserLoggedIn bool   `json:"userLoggedIn"`

	// Update fields
	UpdateReady      bool   `json:"updateReady"`
	UpdateVersion    string `json:"updateVersion"`
	UpdateProgress   int64  `json:"updateProgress"`
	UpdateInstalling bool   `json:"updateInstalling"`
}

// Target is the capture target the picker should mark as live.
type Target struct {
	Kind   string `json:"kind"`
	Index  int    `json:"index"`
	Handle string `json:"handle,omitempty"`
}

// Controller is what the tray needs from the application.
type Controller interface {
	Status() Status
	ToggleSharing()
	OpenSettings()
	OpenPicker() // Opens the window picker to start sharing
	CopyViewerLink()
	CheckForUpdates()
	InstallUpdate()
	Quit()
}

// Options configures a Tray.
type Options struct {
	Logger     *slog.Logger
	Controller Controller
	Title      string
	// OnReady runs once the icon is on screen. Errors are logged and reported
	// to the user as a notification, but do not stop the tray.
	OnReady func() error
}

// Tray is the notification area icon.
type Tray struct {
	logger *slog.Logger
	ctrl   Controller
	title  string
	opts   Options

	mStatus        *systray.MenuItem
	mStartStop     *systray.MenuItem
	mSettings      *systray.MenuItem
	mUser          *systray.MenuItem // User info display
	mCheckUpdates  *systray.MenuItem
	mInstallUpdate *systray.MenuItem
	mQuit          *systray.MenuItem

	// mu guards ready: menu items and the icon only exist after systray.Run has
	// called onReady, but Refresh and Quit may be called before that.
	mu    sync.RWMutex
	ready bool
	// readyCh is closed the moment the tray becomes ready, so a caller that
	// needs to act on the tray (rather than on its state) can wait for it.
	readyCh   chan struct{}
	readyOnce sync.Once
}

// New creates a Tray. Call Run to show it.
func New(opts Options) *Tray {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	title := opts.Title
	if title == "" {
		title = "Streamy"
	}
	return &Tray{
		logger:  logger,
		ctrl:    opts.Controller,
		title:   title,
		opts:    opts,
		readyCh: make(chan struct{}),
	}
}

// Run shows the tray icon and blocks until the user picks Quit or ctx is
// cancelled.
//
// It must be called from the program's main goroutine. On Windows the tray owns
// a window and pumps its message loop; on macOS AppKit requires the main thread
// for anything that touches the UI. Keeping the blocking call on main also
// means the process has a natural "anchor" goroutine that the rest of the app
// hangs off, which makes shutdown ordering obvious.
func (t *Tray) Run(ctx context.Context) error {
	// A second goroutine watches for cancellation and tells the tray to close.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// systray.Quit() needs the tray window to exist, and it can only
			// ever take effect once in the life of the process, so wait until
			// the tray is up before asking it to close.
			select {
			case <-t.readyCh:
			case <-stopped:
				return
			}
			t.logger.Debug("context cancelled, closing tray")
			t.Quit()
		case <-stopped:
		}
	}()

	systray.Run(t.onReady, t.onExit)
	close(stopped)
	return nil
}

// Quit asks the tray to close and Run to return.
func (t *Tray) Quit() {
	t.mu.RLock()
	ready := t.ready
	t.mu.RUnlock()

	if !ready {
		// systray's quit() is wrapped in a sync.Once, so spending it before the
		// tray exists would make the next — the real — Quit a silent no-op and
		// leave the message loop running forever. Doing nothing here is the
		// correct move: the run loop is not running yet either.
		t.logger.Debug("tray quit requested before it was ready, ignoring")
		return
	}
	systray.Quit()
}

// Notify shows a balloon notification next to the tray icon.
func (t *Tray) Notify(title, message string) {
	notify.Show(t.logger, title, message)
}

func (t *Tray) onReady() {
	systray.SetTitle(t.title)
	systray.SetTooltip(t.title)
	systray.SetIcon(IconIdle())

	// A disabled item is a tidy way to show plain text in a menu.
	t.mStatus = systray.AddMenuItem(t.title, "")
	t.mStatus.Disable()
	t.mStartStop = systray.AddMenuItem("Start sharing", "Start or stop capturing the screen")
	t.mSettings = systray.AddMenuItem("Open settings\u2026", "Hotkey, encoder, servers")
	// User info display (disabled item showing user name)
	t.mUser = systray.AddMenuItem("", "User info")
	t.mUser.Disable()
	t.mUser.Hide() // Hidden until user logs in
	t.mCheckUpdates = systray.AddMenuItem("Check for updates\u2026", "Check for a new version")
	t.mInstallUpdate = systray.AddMenuItem("Install Update", "Install the downloaded update")
	t.mInstallUpdate.Hide() // Hidden until update is ready
	systray.AddSeparator()
	t.mQuit = systray.AddMenuItem("Quit", "Stop sharing and exit")

	t.mu.Lock()
	t.ready = true
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.readyCh) })

	go t.clickLoop()
	t.Refresh()

	if t.opts.OnReady != nil {
		if err := t.opts.OnReady(); err != nil {
			t.logger.Error("startup failed", "error", err)
			t.Notify(t.title, "Startup problem: "+err.Error())
		}
	}
}

func (t *Tray) onExit() {
	t.logger.Debug("tray closed")
}

// clickLoop is one goroutine for all menu items: selecting on several channels
// is cheaper and easier to reason about than a goroutine per item.
func (t *Tray) clickLoop() {
	for {
		select {
		case <-t.mStartStop.ClickedCh:
			status := t.ctrl.Status()
			if status.Sharing {
				t.ctrl.ToggleSharing() // Stop sharing
			} else {
				t.ctrl.OpenPicker() // Open window picker to start sharing
			}
		case <-t.mSettings.ClickedCh:
			t.ctrl.OpenSettings()
		case <-t.mCheckUpdates.ClickedCh:
			// Show loading state in the tray menu
			t.mCheckUpdates.SetTitle("Checking for updates...")
			t.mCheckUpdates.Disable()
			go func() {
				t.ctrl.CheckForUpdates()
				// Wait for the check to complete and notifications to show
				time.Sleep(2 * time.Second)
				t.Refresh() // Restore menu item state
			}()
		case <-t.mInstallUpdate.ClickedCh:
			t.ctrl.InstallUpdate()
		case <-t.mQuit.ClickedCh:
			// Quit the app, not just the tray: the app stops the stream and
			// then closes the tray, so no resource is left behind.
			t.ctrl.Quit()
			return
		}
		t.Refresh()
	}
}

// Refresh re-reads the controller status and updates the icon, the menu labels
// and the tooltip. It is safe to call from any goroutine and before Run.
func (t *Tray) Refresh() {
	t.mu.RLock()
	ready := t.ready
	t.mu.RUnlock()
	if !ready {
		return // nothing to update yet; onReady will render the first state
	}

	status := t.ctrl.Status()
	word := actionWord(status.Active)

	if status.Sharing {
		systray.SetIcon(IconLive())
		t.mStartStop.SetTitle("Stop " + word)
	} else {
		systray.SetIcon(IconIdle())
		t.mStartStop.SetTitle("Start sharing")
	}

	text := statusText(status, word)

	// Handle user display
	if status.UserLoggedIn && status.UserName != "" {
		t.mUser.SetTitle("\U0001F464 " + status.UserName)
		t.mUser.Show()
	} else {
		t.mUser.Hide()
	}

	// Handle update status
	if status.UpdateInstalling {
		text = "Installing update..."
		if status.UpdateProgress > 0 {
			text = fmt.Sprintf("Installing update... (%d KB)", status.UpdateProgress/1024)
		}
	} else if status.UpdateReady {
		text = fmt.Sprintf("Update ready: %s", status.UpdateVersion)
		t.mInstallUpdate.Show()
	} else {
		t.mInstallUpdate.Hide()
	}

	t.mStatus.SetTitle(text)

	tooltip := t.title + " \u2014 " + text
	if status.ViewerURL != "" {
		tooltip += "\n" + status.ViewerURL
	}
	if status.UpdateReady {
		tooltip += fmt.Sprintf("\nUpdate %s ready to install", status.UpdateVersion)
	}
	if status.UpdateInstalling {
		tooltip += "\nInstalling update..."
	}
	systray.SetTooltip(tooltip)
}

// actionWord is the verb the menu uses: the pipeline's own word for what it is
// doing, lowercased. Phase 2 records, so the menu offers to "Start recording";
// phase 3 streams, and the same code will say "Start streaming" without being
// touched.
func actionWord(active string) string {
	if active == "" {
		return "sharing"
	}
	return strings.ToLower(active)
}

// statusText is the line at the top of the menu. The controller normally
// supplies one; this is the fallback, and it exists as a function purely so it
// can be tested without a tray icon.
func statusText(status Status, word string) string {
	if status.Text != "" {
		return status.Text
	}
	if status.Sharing {
		if status.Active != "" {
			return status.Active
		}
		return "Sharing"
	}
	return "" // No "Not sharing" text
}

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"streamy/internal/capture"
	"streamy/internal/config"
	"streamy/internal/pipeline"
)

// fakeStreamer records what the app asked it to do, so the tests can check the
// lifecycle without ffmpeg or a network. Implementing an interface is all it
// takes to stand in for the real pipeline — that is the payoff for keeping the
// pipeline behind pipeline.Streamer.
type fakeStreamer struct {
	mu         sync.Mutex
	session    pipeline.Session
	startErr   error
	stopErr    error
	switchErr  error
	starts     int
	stops      int
	switches   int
	lastReq    pipeline.Request
	lastTarget capture.Target
}

func (f *fakeStreamer) Start(_ context.Context, req pipeline.Request) (pipeline.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.lastReq = req
	if f.startErr != nil {
		return pipeline.Session{}, f.startErr
	}
	return f.session, nil
}

func (f *fakeStreamer) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return f.stopErr
}

func (f *fakeStreamer) SwitchTarget(_ context.Context, target capture.Target) (pipeline.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switches++
	f.lastTarget = target
	if f.switchErr != nil {
		return pipeline.Session{}, f.switchErr
	}
	session := f.session
	session.Target = &target
	return session, nil
}

func (f *fakeStreamer) ViewerCount() int { return 0 }

func (f *fakeStreamer) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// testViewerURL is what the fake pipeline hands back as a link.
const testViewerURL = "https://viewer.example/?room=abc"

// sharingSession is the session the fake reports by default.
func sharingSession() pipeline.Session {
	return pipeline.Session{
		ViewerURL:   testViewerURL,
		Label:       "Sharing",
		Description: "Sharing monitor 0",
	}
}

// recordingSession is a phase 2 style session: recording to a local file, with
// the file's URL as the link.
func recordingSession() pipeline.Session {
	return pipeline.Session{
		LocalRecording: `C:\Users\me\Videos\Streamy\streamy-2026-01-01_12-00-00.mp4`,
		ViewerURL:      "file:///C:/Users/me/Videos/Streamy/streamy-2026-01-01_12-00-00.mp4",
		Label:          "Recording",
		Description:    "Recording monitor 2 to C:\\Users\\me\\Videos\\Streamy\\streamy-2026-01-01_12-00-00.mp4",
	}
}

// newTestApp builds a headless app with a substituted clipboard so running the
// tests never touches the developer's real clipboard.
func newTestApp(t *testing.T, streamer pipeline.Streamer) (*App, *[]string) {
	t.Helper()

	var copied []string
	var mu sync.Mutex

	a, err := New(Options{
		Config:   config.Default(),
		Streamer: streamer,
		Headless: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.copyToClipboard = func(text string) error {
		mu.Lock()
		defer mu.Unlock()
		copied = append(copied, text)
		return nil
	}
	return a, &copied
}

// TestRecordingSessionWording checks the phase 2 path: the tray must say
// "Recording", not "Sharing", when the pipeline is writing a file, and the link
// it copies is the recording's own file URL.
func TestRecordingSessionWording(t *testing.T) {
	streamer := &fakeStreamer{session: recordingSession()}
	a, copied := newTestApp(t, streamer)

	a.ToggleSharing()

	status := a.Status()
	if !status.Sharing {
		t.Fatalf("status = %+v, want active", status)
	}
	if status.Text != "Recording" {
		t.Errorf("tray text = %q, want \"Recording\"", status.Text)
	}
	// The menu is built from this, so the user sees "Stop recording" rather
	// than "Stop sharing".
	if status.Active != "Recording" {
		t.Errorf("tray Active = %q, want \"Recording\"", status.Active)
	}
	if status.ViewerURL != recordingSession().ViewerURL {
		t.Errorf("viewer url = %q, want the recording's file URL", status.ViewerURL)
	}
	if len(*copied) != 1 || (*copied)[0] != recordingSession().ViewerURL {
		t.Errorf("clipboard = %v, want the recording's file URL", *copied)
	}
}

func TestToggleSharingStartAndStop(t *testing.T) {
	streamer := &fakeStreamer{session: sharingSession()}
	a, copied := newTestApp(t, streamer)

	if got := a.Status(); got.Sharing || got.Text != StateIdle.String() {
		t.Fatalf("initial status = %+v, want idle", got)
	}

	a.ToggleSharing()

	status := a.Status()
	if !status.Sharing {
		t.Fatalf("status after start = %+v, want sharing", status)
	}
	if status.ViewerURL != testViewerURL {
		t.Errorf("viewer url = %q, want %q", status.ViewerURL, testViewerURL)
	}
	if len(*copied) != 1 || (*copied)[0] != testViewerURL {
		t.Errorf("clipboard = %v, want [%q]", *copied, testViewerURL)
	}

	a.ToggleSharing()

	status = a.Status()
	if status.Sharing || status.ViewerURL != "" {
		t.Errorf("status after stop = %+v, want idle without a link", status)
	}

	starts, stops := streamer.counts()
	if starts != 1 || stops != 1 {
		t.Errorf("starts/stops = %d/%d, want 1/1", starts, stops)
	}
}

// Phase 1 ships the stub, which refuses to start. The app must stay idle and not
// pretend a stream is running.
func TestNotImplementedStreamerLeavesAppIdle(t *testing.T) {
	streamer := &fakeStreamer{startErr: pipeline.ErrNotImplemented}
	a, copied := newTestApp(t, streamer)

	a.ToggleSharing()

	status := a.Status()
	if status.Sharing {
		t.Errorf("status = %+v, want idle after a failed start", status)
	}
	if status.ViewerURL != "" {
		t.Errorf("viewer url = %q, want empty", status.ViewerURL)
	}
	if len(*copied) != 0 {
		t.Errorf("clipboard = %v, want nothing copied", *copied)
	}
}

func TestStartFailureIsReportedAndRecoverable(t *testing.T) {
	streamer := &fakeStreamer{startErr: errors.New("ffmpeg not found")}
	a, _ := newTestApp(t, streamer)

	a.ToggleSharing()
	if got := a.Status(); got.Sharing {
		t.Fatalf("status = %+v, want idle after a failed start", got)
	}

	// A later attempt must be allowed: the failure did not wedge the state.
	streamer.mu.Lock()
	streamer.startErr = nil
	streamer.session = pipeline.Session{ViewerURL: "https://viewer.example/?room=retry", Label: "Sharing"}
	streamer.mu.Unlock()

	a.ToggleSharing()
	if got := a.Status(); !got.Sharing {
		t.Errorf("status = %+v, want sharing on the retry", got)
	}
}

func TestCopyViewerLinkWithoutStream(t *testing.T) {
	streamer := &fakeStreamer{session: sharingSession()}
	a, copied := newTestApp(t, streamer)
	a.CopyViewerLink()
	if len(*copied) != 0 {
		t.Errorf("clipboard = %v, want nothing copied while idle", *copied)
	}
}

func TestSwitchWhileSharingKeepsLinkAndUpdatesTarget(t *testing.T) {
	streamer := &fakeStreamer{session: sharingSession()}
	a, _ := newTestApp(t, streamer)

	a.ToggleSharing()
	if got := a.Status(); !got.Sharing {
		t.Fatalf("status after start = %+v, want sharing", got)
	}

	target := capture.Target{
		Kind:   capture.KindWindow,
		Window: capture.Window{Handle: 42, Title: "Editor"},
	}
	if err := a.SwitchSharing(target); err != nil {
		t.Fatalf("SwitchSharing: %v", err)
	}

	status := a.Status()
	if !status.Sharing {
		t.Fatalf("status after switch = %+v, want still sharing", status)
	}
	if status.ViewerURL != testViewerURL {
		t.Errorf("viewer url = %q, want the original room link %q", status.ViewerURL, testViewerURL)
	}
	if status.Target == nil || status.Target.Kind != "window" || status.Target.Handle != "42" {
		t.Errorf("active target = %+v, want window handle 42", status.Target)
	}
	if streamer.switches != 1 {
		t.Errorf("switch calls = %d, want 1", streamer.switches)
	}
}

func TestFindUIExecutable(t *testing.T) {
	t.Run("built UI beside the tray takes precedence", func(t *testing.T) {
		root := t.TempDir()
		binDir := filepath.Join(root, "bin")
		writeTestFile(t, filepath.Join(binDir, "streamy.exe"))
		sibling := writeTestFile(t, filepath.Join(binDir, "streamy-ui.exe"))

		if got := findUIExecutable(filepath.Join(binDir, "streamy.exe"), root); got != sibling {
			t.Fatalf("findUIExecutable() = %q, want sibling %q", got, sibling)
		}
	})

	t.Run("go run finds the UI in a module checkout", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "go.mod"))
		writeTestFile(t, filepath.Join(root, "ui", "main.go"))
		ui := writeTestFile(t, filepath.Join(root, "out", "bin", "streamy-ui.exe"))

		if got := findUIExecutable(filepath.Join(t.TempDir(), "streamy.exe"), root); got != ui {
			t.Fatalf("findUIExecutable() = %q, want development build %q", got, ui)
		}
	})

	t.Run("does not search arbitrary working directories", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "out", "bin", "streamy-ui.exe"))

		if got := findUIExecutable(filepath.Join(t.TempDir(), "streamy.exe"), root); got != "" {
			t.Fatalf("findUIExecutable() = %q, want no UI outside a module checkout", got)
		}
	})
}

func writeTestFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRedactViewerURLRemovesRoomAndTURNSecrets(t *testing.T) {
	got := redactViewerURL("https://watch.example/live?auth=query-secret#room=secret&ice=password")
	if got != "https://watch.example" {
		t.Fatalf("redactViewerURL = %q, want only the public viewer origin", got)
	}
}

func TestQuitIsIdempotent(t *testing.T) {
	a, _ := newTestApp(t, &fakeStreamer{})

	done := make(chan struct{})
	go func() {
		a.Quit()
		a.Quit() // must not panic on a double close
		close(done)
	}()
	<-done

	select {
	case <-a.quitCh:
	default:
		t.Error("quitCh was not closed by Quit()")
	}
}

func TestShutdownStopsAnActiveStream(t *testing.T) {
	streamer := &fakeStreamer{session: sharingSession()}
	a, _ := newTestApp(t, streamer)

	a.ToggleSharing()
	if !a.Status().Sharing {
		t.Fatal("expected to be sharing before shutdown")
	}

	if err := a.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	_, stops := streamer.counts()
	if stops != 1 {
		t.Errorf("stops = %d, want 1", stops)
	}
	if got := a.Status(); got.Sharing {
		t.Errorf("status after shutdown = %+v, want idle", got)
	}
}

func TestStateString(t *testing.T) {
	// Guards the strings the tray shows; if one changes, the test says so.
	if got := StateIdle.String(); got != "Not sharing" {
		t.Errorf("StateIdle = %q, want \"Not sharing\"", got)
	}
	if got := StateStreaming.String(); got != "Sharing" {
		t.Errorf("StateStreaming = %q, want \"Sharing\"", got)
	}
}

// TestRunShutsDownOnContextCancel walks the whole headless lifecycle: onReady
// runs, the hotkey is registered, the context is cancelled and everything is
// released again. This is the phase 1 "clean shutdown" requirement in test form.
func TestRunShutsDownOnContextCancel(t *testing.T) {
	streamer := &fakeStreamer{session: sharingSession()}
	a, err := New(Options{
		Config:          config.Default(), // hotkey enabled
		Streamer:        streamer,
		Headless:        true,
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.copyToClipboard = func(string) error { return nil }

	// Pretend the user already pressed the hotkey once.
	a.ToggleSharing()
	if !a.Status().Sharing {
		t.Fatal("expected to be sharing before Run")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Give onReady a moment to register the hotkey, then ask the app to stop.
	// (Cancelling before onReady would race shutdown against startup; the app
	// is only reachable through a signal or the tray in real life, so that race
	// cannot happen in practice.)
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	starts, stops := streamer.counts()
	if starts != 1 || stops != 1 {
		t.Errorf("starts/stops = %d/%d, want 1/1", starts, stops)
	}
	if got := a.hotkeys.Spec(); got != "" {
		t.Errorf("hotkey %q is still registered after shutdown", got)
	}
	if got := a.Status(); got.Sharing {
		t.Errorf("status after shutdown = %+v, want idle", got)
	}
}

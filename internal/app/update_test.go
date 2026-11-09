package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"streamy/internal/config"
)

func newUpdateTestApp(t *testing.T) *App {
	t.Helper()
	a, err := New(Options{Config: config.Default(), ConfigPath: filepath.Join(t.TempDir(), "config.json"), Headless: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestUpdateOperationIsSingleFlight(t *testing.T) {
	a := newUpdateTestApp(t)
	if !a.beginUpdateOperation() {
		t.Fatal("first update operation was rejected")
	}
	if a.beginUpdateOperation() {
		t.Fatal("second update operation was not coalesced")
	}
	a.finishUpdateIdle()
	if a.updateInFlight || a.updatePhase != updateIdle {
		t.Fatalf("operation did not return to idle: inFlight=%v phase=%q", a.updateInFlight, a.updatePhase)
	}
}

func TestUpdateInstallWaitsForIdleAndClaimsOnce(t *testing.T) {
	a := newUpdateTestApp(t)
	file := filepath.Join(t.TempDir(), "update.exe")
	writeTestFile(t, file)
	called := make(chan string, 1)
	a.launchUpdateHelperFn = func(path string) error {
		called <- path
		return nil
	}
	a.mu.Lock()
	a.updateInFlight = true
	a.updatePhase = updateDownloading
	a.pendingUpdateFile = file
	a.pendingUpdateVersion = "v2.0.0"
	a.state = StateStreaming
	a.mu.Unlock()

	a.tryStartUpdateInstall()
	a.mu.Lock()
	phase := a.updatePhase
	claimed := a.updateInstallClaimed
	a.mu.Unlock()
	if phase != updateWaiting || claimed {
		t.Fatalf("active stream should defer install: phase=%q claimed=%v", phase, claimed)
	}

	a.mu.Lock()
	a.state = StateIdle
	a.mu.Unlock()
	a.tryStartUpdateInstall()
	select {
	case got := <-called:
		if got != file {
			t.Fatalf("helper path = %q, want %q", got, file)
		}
	case <-time.After(time.Second):
		t.Fatal("idle transition did not start installer")
	}
	// A second request cannot launch a second helper.
	a.tryStartUpdateInstall()
	select {
	case extra := <-called:
		t.Fatalf("installer launched twice for %q", extra)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestDownloadInstallerTracksBytesAndRejectsHTML(t *testing.T) {
	a := newUpdateTestApp(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/streamy.exe":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("MZ test installer"))
		case "/release.exe":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>release page</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a.updateHTTPClient = server.Client()

	first, firstSize, err := a.downloadInstaller(server.URL + "/streamy.exe")
	if err != nil {
		t.Fatalf("first download: %v", err)
	}
	second, secondSize, err := a.downloadInstaller(server.URL + "/streamy.exe")
	if err != nil {
		t.Fatalf("second download: %v", err)
	}
	if first == second || firstSize != int64(len("MZ test installer")) || secondSize != firstSize {
		t.Fatalf("downloads = (%q, %d), (%q, %d)", first, firstSize, second, secondSize)
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.downloadInstaller(server.URL + "/release.exe"); err == nil {
		t.Fatal("HTML response was accepted as an installer")
	}
}

func TestIPCUpdateReportsProgressAndTerminalError(t *testing.T) {
	a := newUpdateTestApp(t)
	if err := a.startIPC(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.stopIPC(context.Background()) })
	a.mu.Lock()
	a.updatePhase = updateFailed
	a.updateProgress = 12
	a.updateTotal = 100
	a.pendingUpdateVersion = "v2.0.0"
	a.updateError = "installer handoff failed"
	a.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, "http://"+a.ipc.address+"/api/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.ipc.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["phase"] != string(updateFailed) || payload["error"] != "installer handoff failed" || payload["failed"] != true {
		t.Fatalf("unexpected update payload: %#v", payload)
	}
	if payload["total"] != float64(100) || payload["progress"] != float64(12) {
		t.Fatalf("progress payload = %#v", payload)
	}
}

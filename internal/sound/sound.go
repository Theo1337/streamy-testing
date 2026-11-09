// Package sound plays the short cues that mark session and viewer events.
//
// The files are embedded so the tray executable keeps working when it is moved
// on its own, and playback is best effort: a machine without an audio device
// must never fail a stream.
package sound

import (
	"bytes"
	"embed"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

//go:embed assets/*.mp3
var assets embed.FS

const viewerCueGap = time.Second

var (
	extractMu sync.Mutex
	extracted = map[string]string{}

	cueMu   sync.Mutex
	lastCue = map[string]time.Time{}
)

// PlayStart plays the cue used when a session starts. It returns immediately.
func PlayStart() { play("start.mp3") }

// PlayStop plays the cue used when a session stops. It returns immediately.
func PlayStop() { play("stop.mp3") }

// PlayViewerJoined plays the cue used when a viewer starts watching. It returns
// immediately.
func PlayViewerJoined() { playViewerCue("join.mp3") }

// PlayViewerLeft plays the cue used when a viewer stops watching. It returns
// immediately.
func PlayViewerLeft() { playViewerCue("leave.mp3") }

// playViewerCue collapses a burst of viewer events into one cue. Several people
// joining at once should sound like a door, not a stutter.
func playViewerCue(name string) {
	if !claimCue(name, time.Now()) {
		return
	}
	play(name)
}

// claimCue reports whether name may play now and records the attempt. It is
// split out so the coalescing rule can be tested without touching audio
// hardware.
func claimCue(name string, now time.Time) bool {
	cueMu.Lock()
	defer cueMu.Unlock()
	if last, ok := lastCue[name]; ok && now.Sub(last) < viewerCueGap {
		return false
	}
	lastCue[name] = now
	return true
}

func play(name string) {
	go func() {
		if err := playFile(name); err != nil {
			slog.Debug("could not play sound cue", "sound", name, "error", err)
		}
	}()
}

// materialize writes an embedded sound to the user's cache directory and
// returns its path. Windows' MCI player works on files, not byte slices.
func materialize(name string) (string, error) {
	extractMu.Lock()
	defer extractMu.Unlock()
	if path, ok := extracted[name]; ok {
		return path, nil
	}

	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return "", err
	}

	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "Streamy", "sounds")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	path := filepath.Join(dir, name)
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		extracted[name] = path
		return path, nil
	}

	// Write beside the destination first so a half-written file is never
	// handed to the player.
	tmp, err := os.CreateTemp(dir, ".streamy-sound-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return "", err
	}

	extracted[name] = path
	return path, nil
}

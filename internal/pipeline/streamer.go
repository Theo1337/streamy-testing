// Package pipeline glues capture and encode together into the thing the tray
// app actually drives.
//
// The split from internal/app is deliberate. app owns the lifecycle of the
// process: the tray, the hotkey and the config file. pipeline owns the
// lifecycle of one capture. Nothing in this package imports app, so the
// dependency runs one way only.
package pipeline

import (
	"context"
	"errors"
	"log/slog"

	"streamy/internal/capture"
	"streamy/internal/config"
)

// Streamer is the capture -> encode -> output pipeline behind one pair of
// methods. The live streamer is used by the tray; Recorder remains available
// for diagnostics and record-only runs.
//
// Defining it here rather than in app means the package that implements it also
// owns the contract, and app only has to depend on this package. Phase 3 adds
// the WebRTC streamer behind the same two methods.
type Streamer interface {
	// Start begins capturing and returns what the user needs to know about the
	// session.
	Start(ctx context.Context, req Request) (Session, error)

	// Stop tears the pipeline down. It must be safe to call when nothing is
	// running, and it must leave no ffmpeg process behind.
	Stop(ctx context.Context) error

	// SwitchTarget changes what is being captured without closing the live
	// session. Viewers keep the same link and connection; the encoder starts a
	// new stream and the broadcaster requests a keyframe.
	SwitchTarget(ctx context.Context, target capture.Target) (Session, error)

	// ViewerCount returns the current number of connected viewers.
	// Returns 0 if not applicable (e.g., recording only).
	ViewerCount() int
}

// Request is everything a Streamer needs to start.
type Request struct {
	// Config is a snapshot of the settings to use.
	Config config.Config
	// Target, when set, is an explicit choice from the on-demand picker.
	Target *capture.Target
	// IdentityToken is a Worker-signed Discord host identity.
	IdentityToken string
	// UserID is the Discord user ID, used to resolve the voice channel the
	// streamer is currently in for Discord Activity.
	UserID string
	// UserName is the Discord display name the bot puts in its message.
	UserName string
	// OnViewerWatching is called once per viewer when their WebRTC connection
	// is established, meaning video is actually reaching them. It runs on its
	// own goroutine and must not block. Optional.
	OnViewerWatching func()
	// OnViewerLeft is called when a viewer that was watching disconnects.
	// It runs on the signaling goroutine, so it must not block. Optional.
	OnViewerLeft func()
}

// Session describes a running pipeline.
type Session struct {
	// ViewerURL is the link to hand to whoever wants to watch. The live streamer
	// puts the room link here. Recorder puts a file:// URL to the recording
	// here instead of a placeholder or an empty string, because that link
	// really does open the thing that is being recorded.
	ViewerURL string

	// LocalRecording is the file being written, empty when the pipeline is not
	// recording to disk.
	LocalRecording string

	// Label is a short word for the tray: "Sharing" or "Recording".
	Label string

	// Description is one sentence for the notification: what is being captured,
	// and where it is going.
	Description string
	// Target is the resolved capture target, so the picker can mark the live
	// card even when the stream was started from the hotkey.
	Target *capture.Target
}

// ErrNotImplemented marks a Streamer that is still a placeholder.
var ErrNotImplemented = errors.New("the capture pipeline is not implemented yet")

// Stub is the placeholder used when no pipeline is configured.
//
// It refuses to start with a clear error rather than pretending that video is
// flowing, so a notification on the hotkey means "the hotkey worked" and not
// "the stream worked".
type Stub struct {
	logger *slog.Logger
}

// NewStub returns a placeholder pipeline.
func NewStub(logger *slog.Logger) *Stub {
	return &Stub{logger: logger}
}

// Start always fails with ErrNotImplemented.
func (s *Stub) Start(context.Context, Request) (Session, error) {
	return Session{}, ErrNotImplemented
}

// Stop does nothing, and is happy about it.
func (s *Stub) Stop(context.Context) error { return nil }

// SwitchTarget is only meaningful for a running pipeline.
func (s *Stub) SwitchTarget(context.Context, capture.Target) (Session, error) {
	return Session{}, ErrNotImplemented
}

func (s *Stub) ViewerCount() int { return 0 }

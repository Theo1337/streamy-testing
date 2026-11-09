package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"streamy/internal/capture"
	"streamy/internal/config"
	"streamy/internal/encode"
)

// startupGrace is how long ffmpeg is given to prove it is alive.
//
// ffmpeg reports an unusable encoder, a missing capture device or a bad option
// by exiting within a few hundred milliseconds. Waiting before declaring
// success is the difference between "recording started" being true and being
// merely optimistic. Half a second covers those failures without making the
// hotkey feel slow; a process that is still alive after that is capturing.
const startupGrace = 500 * time.Millisecond

// switchPollInterval is how often the auto switch looks for a changed target.
const switchPollInterval = 2 * time.Second

// Recorder records the resolved capture target to a file.
//
// It is the phase 2 pipeline. Its purpose is to prove the whole chain end to
// end — target resolution, ffmpeg arguments, encoder selection, the
// subprocess, and a shutdown that leaves a playable file — before WebRTC is
// layered on top. It can also hand the encoded stream to a writer instead of a
// file, which is the shape phase 3 needs and what the debug tool's -stdout flag
// exercises.
type Recorder struct {
	opts   Options
	logger *slog.Logger

	// Setup is done once, on the first Start, because finding ffmpeg and
	// probing encoders costs about a second and most runs of the tray process
	// never share anything at all.
	ffmpegOnce sync.Once
	ffmpeg     encode.FFmpeg
	ffmpegErr  error

	capsOnce sync.Once
	caps     capture.Capabilities

	resolverOnce sync.Once
	resolver     capture.TargetResolver

	// mu guards everything below.
	mu          sync.Mutex
	runner      *encode.Runner
	session     Session
	target      capture.Target
	encoder     encode.Encoder
	cfg         config.Config
	cancelRun   context.CancelFunc
	watcher     chan struct{}
	watchCancel context.CancelFunc
	pumpDone    chan struct{}
	frameSource io.Closer
}

// Options configures a Recorder.
type Options struct {
	// Logger may be nil.
	Logger *slog.Logger

	// OutputDir is where recordings are written. Empty means the Streamy folder
	// inside the user's Videos directory.
	OutputDir string

	// Format is the container to write: "mp4", "mkv", "mp4-frag" or "h264".
	// Empty means "mp4", or "h264" when Stdout is set.
	Format string

	// Stdout, when set, receives the encoded stream instead of a file, and the
	// recorder writes nothing to disk.
	Stdout io.Writer
	// CloseStdout closes Stdout after ffmpeg's output is drained. This is useful
	// for an io.PipeWriter, where close signals EOF to the reader.
	CloseStdout bool
}

// NewRecorder returns a Recorder.
func NewRecorder(opts Options) *Recorder {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{opts: opts, logger: logger}
}

// SetStdout makes the next Start write the encoded stream to w instead of a
// file, and close w afterwards when closeAfter is set.
//
// A Recorder is reused across sessions so its one-time probes are paid once,
// but the pipe that carries one stream is worthless to the next one, so the
// writer has to be replaced before each Start. It is not safe to call while a
// stream is running.
func (r *Recorder) SetStdout(w io.Writer, closeAfter bool) {
	r.opts.Stdout = w
	r.opts.CloseStdout = closeAfter
}

// Start resolves the target, starts ffmpeg and returns the session.
func (r *Recorder) Start(ctx context.Context, req Request) (Session, error) {
	r.mu.Lock()
	if r.runner != nil {
		current := r.session
		r.mu.Unlock()
		return current, fmt.Errorf("already recording to %s", current.LocalRecording)
	}
	r.mu.Unlock()

	// Everything up to here runs inside the caller's context, which bounds how
	// long starting may take.
	ffmpeg, err := r.ensureFFmpeg(ctx, req.Config)
	if err != nil {
		return Session{}, err
	}

	encoder, err := encode.Select(ctx, ffmpeg, req.Config, r.logger)
	if err != nil {
		return Session{}, err
	}

	// The context given to Start only bounds the start-up work: the caller
	// cancels it as soon as Start returns. The capture itself outlives it, so
	// the runner gets its own context, derived from the caller's values but not
	// from its cancellation, and Stop cancels that.
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))

	session, err := r.startCapture(runCtx, req.Config, ffmpeg, encoder, req.Target, true)
	if err != nil {
		cancelRun()
		return Session{}, err
	}

	r.mu.Lock()
	r.cancelRun = cancelRun
	r.mu.Unlock()

	return session, nil
}

// preparedCapture is a resolved target whose ffmpeg input has already been
// built and validated. Preparing before starting is what lets a switch reject
// an uncapturable target without stopping the healthy capture it replaces.
type preparedCapture struct {
	target       capture.Target
	grabber      capture.Grabber
	capabilities capture.Capabilities
	input        *encode.InputOverride
	frameSource  io.Closer
}

// prepareCapture resolves the target and builds its ffmpeg input without
// starting ffmpeg.
//
// Start and the switch both go through it. The only side effect is a window
// frame source, which the caller owns and must close if the capture is not
// started; everything else is pure argument building.
func (r *Recorder) prepareCapture(ctx context.Context, cfg config.Config, ffmpeg encode.FFmpeg, encoder encode.Encoder, requested *capture.Target) (preparedCapture, error) {
	capabilities := r.capabilities(ctx, ffmpeg, cfg)

	var target capture.Target
	if requested != nil {
		target = *requested
		target.Cursor = true // cursor is always shown since the setting was removed
		if target.Kind == capture.KindWindow && target.Monitor.Bounds.Empty() {
			// A window is recorded as the rectangle it occupies on the
			// composited desktop; knowing its monitor allows the cheaper
			// duplicate-and-crop path.
			if desktop, err := r.targetResolver().Snapshot(ctx); err == nil {
				target = target.WithDesktop(desktop)
			}
		}
	} else {
		var err error
		target, err = r.targetResolver().Resolve(ctx, cfg)
		if err != nil {
			return preparedCapture{}, err
		}
	}
	grabber := target.ChooseGrabber(capabilities)
	var inputOverride *encode.InputOverride
	var frameSource io.Closer
	if target.Kind == capture.KindWindow && capture.WindowCaptureSupported() {
		// A window is captured through PrintWindow instead of as a rectangle
		// of the composited desktop: only that window's own pixels are sent,
		// even when another window covers it. The frames travel over a
		// loopback TCP socket so ffmpeg's stdin stays free for its graceful
		// "q" shutdown.
		source, err := capture.NewWindowFrameSource(ctx, target.Window, cfg.Encoding.Resolution, cfg.Encoding.FPS, target.Cursor)
		if err != nil {
			r.logger.Warn("window capture is unavailable; falling back to the desktop region", "error", err)
		} else {
			grabber = capture.GrabberWindow
			frameSource = source
			inputOverride = &encode.InputOverride{
				Args:    source.InputArgs(),
				Filters: capture.WindowFilters(cfg.Encoding.Resolution),
			}
		}
	}

	// Validate the whole input here rather than leaving it to encode.Start:
	// this is the point where a monitor with no DXGI output, an empty window
	// rectangle or a rejected filter has to be reported, before anything is
	// torn down.
	if err := encode.ValidateInput(cfg, target, encoder, capabilities, inputOverride); err != nil {
		if frameSource != nil {
			_ = frameSource.Close()
		}
		return preparedCapture{}, err
	}

	return preparedCapture{
		target:       target,
		grabber:      grabber,
		capabilities: capabilities,
		input:        inputOverride,
		frameSource:  frameSource,
	}, nil
}

// startCapture resolves a target and brings one ffmpeg process up for it.
//
// Start and the auto switch both use it, which is why it does not touch the
// shared cancel function or the watcher.
func (r *Recorder) startCapture(ctx context.Context, cfg config.Config, ffmpeg encode.FFmpeg, encoder encode.Encoder, requested *capture.Target, announce bool) (Session, error) {
	prepared, err := r.prepareCapture(ctx, cfg, ffmpeg, encoder, requested)
	if err != nil {
		return Session{}, err
	}
	return r.startPrepared(ctx, cfg, ffmpeg, encoder, prepared, announce)
}

// startPrepared brings ffmpeg up for an input that prepareCapture has already
// resolved and validated.
func (r *Recorder) startPrepared(ctx context.Context, cfg config.Config, ffmpeg encode.FFmpeg, encoder encode.Encoder, prepared preparedCapture, announce bool) (Session, error) {
	target, grabber := prepared.target, prepared.grabber
	frameSource := prepared.frameSource
	closeFrameSource := func() {
		if frameSource != nil {
			_ = frameSource.Close()
		}
	}

	// Requirement 8 from the spec: always say which target was chosen and why.
	// This line is the first thing to read when the wrong screen got recorded.
	r.logger.Info("capture target chosen",
		"target", target.Describe(),
		"reason", target.Reason,
		"grabber", string(grabber),
		"encoder", encoder.Name,
	)

	output, err := r.output()
	if err != nil {
		closeFrameSource()
		return Session{}, err
	}

	runner, err := encode.Start(ctx, encode.StartOptions{
		FFmpeg:       ffmpeg,
		Config:       cfg,
		Target:       target,
		Encoder:      encoder,
		Capabilities: prepared.capabilities,
		Output:       output,
		Input:        prepared.input,
		Logger:       r.logger,
	})
	if err != nil {
		closeFrameSource()
		return Session{}, err
	}

	if err := runner.WaitForStart(startupGrace); err != nil {
		// It is already dead: tidy up and report why, rather than leaving a
		// corpse behind and telling the user they are recording.
		closeFrameSource()
		_ = runner.Stop(context.Background())
		return Session{}, fmt.Errorf("ffmpeg could not capture %s: %w", target.Describe(), err)
	}

	session := Session{}
	targetCopy := target
	session.Target = &targetCopy
	if output.Pipe {
		session.Label = "Streaming"
		session.Description = fmt.Sprintf("Encoding %s with %s via %s to stdout",
			target.Describe(), encoder.Name, grabber)
	} else {
		// A file:// URL is what goes on the clipboard. It is a real link to a
		// real file, so the tray's "copy viewer link" does something honest
		// even before the viewer page exists.
		session.Label = "Recording"
		session.LocalRecording = output.Path
		session.ViewerURL = fileURL(output.Path)
		session.Description = fmt.Sprintf("Recording %s with %s via %s to %s",
			target.Describe(), encoder.Name, grabber, output.Path)
	}

	var pump chan struct{}
	if output.Pipe && r.opts.Stdout != nil {
		// The phase 3 shape: the encoded stream is handed to a reader instead
		// of being written to disk.
		pump = make(chan struct{})
		go func() {
			defer close(pump)
			if r.opts.CloseStdout {
				defer func() {
					if closer, ok := r.opts.Stdout.(io.Closer); ok {
						if err := closer.Close(); err != nil {
							r.logger.Debug("closing the encoded stream writer", "error", err)
						}
					}
				}()
			}
			if _, err := io.Copy(r.opts.Stdout, runner.Stdout()); err != nil && !errors.Is(err, io.EOF) {
				r.logger.Debug("copying the encoded stream to the writer", "error", err)
			}
		}()
	}

	r.mu.Lock()
	r.runner = runner
	r.target = target
	r.encoder = encoder
	r.cfg = cfg
	r.session = session
	r.pumpDone = pump
	r.frameSource = frameSource
	r.mu.Unlock()

	if announce {
		r.logger.Info("capture started", "target", target.Describe(), "encoder", encoder.Name)
	}
	return session, nil
}

// ensureFFmpeg finds ffmpeg once and remembers the answer, including a failure:
// if ffmpeg is not installed, retrying on every hotkey press would only make
// each press slower.
func (r *Recorder) ensureFFmpeg(ctx context.Context, cfg config.Config) (encode.FFmpeg, error) {
	r.ffmpegOnce.Do(func() {
		r.ffmpeg, r.ffmpegErr = encode.Find(ctx, cfg.Encoding.FFmpegPath)
		if r.ffmpegErr == nil {
			r.logger.Info("using ffmpeg", "path", r.ffmpeg.Path, "version", r.ffmpeg.Version())
		}
	})
	return r.ffmpeg, r.ffmpegErr
}

// capabilities probes what this machine can do, once per process.
//
// It first runs a global ddagrab probe to see if Desktop Duplication works at
// all. If it does, it then probes each monitor output individually to detect
// the Optimus failure mode: ddagrab returns a valid stream of all-black frames
// when the D3D11 device is created on the dGPU but the monitor is driven by
// the iGPU. The per-output results are stored in Capabilities.DDAGrab as a map
// from DXGI output index to usable/unusabe, so ChooseGrabber can fall back to
// GDI only for the affected monitors.
func (r *Recorder) capabilities(ctx context.Context, ffmpeg encode.FFmpeg, cfg config.Config) capture.Capabilities {
	r.capsOnce.Do(func() {
		if err := ffmpeg.ProbeDDAGrab(ctx); err != nil {
			// Not fatal. It only means fullscreen captures fall back to GDI,
			// which is slower and can come out black for exclusive fullscreen
			// games, so it is worth saying out loud.
			r.logger.Warn("Desktop Duplication (ddagrab) is unavailable, falling back to GDI capture; exclusive fullscreen games may record as a black screen",
				"reason", err)
			r.caps = capture.Capabilities{DDAGrab: map[int]int8{}}
			return
		}
		r.logger.Debug("Desktop Duplication (ddagrab) is available, probing per-output health")

		// Get the current desktop snapshot to know which monitors exist and
		// their DXGI output indices.
		desktop, err := r.targetResolver().Snapshot(ctx)
		if err != nil {
			r.logger.Warn("could not snapshot monitors for per-output ddagrab probe; falling back to GDI for all outputs",
				"error", err)
			r.caps = capture.Capabilities{DDAGrab: map[int]int8{}}
			return
		}

		// Probe each monitor with a valid DXGI output index.
		ddagrabOK := make(map[int]int8)
		for _, m := range desktop.Monitors {
			if m.DXGIIndex < 0 {
				continue
			}
			if err := ffmpeg.ProbeDDAGrabOutput(ctx, m.DXGIIndex); err != nil {
				r.logger.Warn("ddagrab failed for monitor output, will use GDI",
					"monitor", m.Index, "dxgi_output", m.DXGIIndex, "error", err)
				ddagrabOK[m.DXGIIndex] = capture.DDAGrabUnhealthy
			} else {
				r.logger.Debug("ddagrab healthy for monitor output",
					"monitor", m.Index, "dxgi_output", m.DXGIIndex)
				ddagrabOK[m.DXGIIndex] = capture.DDAGrabHealthy
			}
		}

		if len(ddagrabOK) == 0 {
			r.logger.Debug("no monitors with valid DXGI outputs to probe")
			r.caps = capture.Capabilities{DDAGrab: map[int]int8{}}
		} else {
			// Set sentinel -1 to indicate global ddagrab availability
			ddagrabOK[-1] = capture.DDAGrabHealthy
			r.caps = capture.Capabilities{DDAGrab: ddagrabOK}
		}
	})
	return r.caps
}

func (r *Recorder) targetResolver() capture.TargetResolver {
	r.resolverOnce.Do(func() {
		r.resolver = capture.NewResolver(r.logger)
	})
	return r.resolver
}

// output returns where the encoded stream should go.
func (r *Recorder) output() (encode.Output, error) {
	if r.opts.Stdout != nil {
		return encode.Output{Format: r.opts.Format, Pipe: true}, nil
	}

	path, err := r.recordingPath(time.Now())
	if err != nil {
		return encode.Output{}, err
	}
	return encode.Output{Path: path, Format: r.opts.Format}, nil
}

// recordingPath builds a fresh path for a recording.
//
// Recordings are user content, so they go in the Videos folder rather than in
// the config directory next to the log. The timestamp is local time and sorts
// in order, which makes a folder of recordings easy to scan.
func (r *Recorder) recordingPath(now time.Time) (string, error) {
	format := r.opts.Format
	if format == "" {
		format = "mp4"
	}

	dir := r.opts.OutputDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the home directory to put recordings in: %w", err)
		}
		dir = filepath.Join(home, "Videos", "Streamy")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create the recordings folder: %w", err)
	}

	ext := "mp4"
	switch format {
	case "mkv":
		ext = "mkv"
	case "h264":
		ext = "h264"
	}

	name := "streamy-" + now.Format("2006-01-02_15-04-05") + "." + ext
	return filepath.Join(dir, name), nil
}

// Stop stops the capture and waits for ffmpeg to finish writing.
func (r *Recorder) Stop(ctx context.Context) error {
	r.mu.Lock()
	runner := r.runner
	cancel := r.cancelRun
	watcher := r.watcher
	watchCancel := r.watchCancel
	pump := r.pumpDone
	session := r.session
	frameSource := r.frameSource
	r.runner = nil
	r.cancelRun = nil
	r.watcher = nil
	r.watchCancel = nil
	r.pumpDone = nil
	r.frameSource = nil
	r.session = Session{}
	r.mu.Unlock()

	if runner == nil {
		if frameSource != nil {
			_ = frameSource.Close()
		}
		return nil
	}

	// Closing the frame source first makes ffmpeg see the end of its input, so
	// it can finish the file before the graceful "q" below.
	if frameSource != nil {
		_ = frameSource.Close()
	}

	// Stop the watcher first, so it cannot start a new capture while this one
	// is being torn down. Cancelling the run context is what makes it exit.
	if cancel != nil {
		cancel()
	}
	if watchCancel != nil {
		watchCancel()
	}
	if watcher != nil {
		select {
		case <-watcher:
		case <-ctx.Done():
		}
	}

	// runner.Stop asks ffmpeg to quit with "q", which is what makes it write
	// the index and leave a playable file behind.
	err := runner.Stop(ctx)

	if pump != nil {
		// Wait for the last bytes to reach the writer before reporting done.
		select {
		case <-pump:
		case <-ctx.Done():
		}
	}

	if session.LocalRecording != "" {
		r.logger.Info("recording finished", "file", session.LocalRecording)
	}
	return err
}

// Session returns the running session, if any.
func (r *Recorder) Session() Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session
}

// FFmpegStderr returns the tail of ffmpeg's stderr for stall detection.
// Returns empty string if no capture is running.
func (r *Recorder) FFmpegStderr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runner == nil {
		return ""
	}
	return r.runner.Stderr()
}

// startWatcher polls for a changed target while the capture runs.
//
// Only started when capture.autoSwitchTarget is on. Switching means stopping
// one ffmpeg and starting another, which is why each target gets its own file
// and why this is off by default.
func (r *Recorder) startWatcher(ctx context.Context, cfg config.Config, ffmpeg encode.FFmpeg, encoder encode.Encoder) {
	done := make(chan struct{})
	watchCtx, watchCancel := context.WithCancel(ctx)

	r.mu.Lock()
	r.watcher = done
	r.watchCancel = watchCancel
	current := r.target
	r.mu.Unlock()

	go func() {
		defer close(done)
		defer watchCancel()

		ticker := time.NewTicker(switchPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
			}

			target, err := r.targetResolver().Resolve(watchCtx, cfg)
			if err != nil {
				r.logger.Debug("auto switch: could not resolve a target", "error", err)
				continue
			}
			if sameTarget(target, current) {
				continue
			}

			r.logger.Info("auto switching capture target",
				"from", current.Describe(), "to", target.Describe(), "reason", target.Reason)

			if _, err := r.switchTo(watchCtx, cfg, ffmpeg, encoder, &target); err != nil {
				// A switch that fails after the old capture was stopped leaves
				// nothing running, so it is reported loudly and the watcher
				// gives up rather than thrashing. A target rejected during
				// preparation keeps the current capture alive.
				r.logger.Error("auto switch failed", "error", err)
				return
			}
			current = target
		}
	}()
}

// SwitchTarget changes what the running capture reads. It is what the picker
// calls when the user clicks another window while already live: the room,
// broadcaster and viewers stay exactly as they were, and only ffmpeg is
// restarted around the new input.
func (r *Recorder) SwitchTarget(ctx context.Context, target capture.Target) (Session, error) {
	r.mu.Lock()
	cfg, ffmpeg, encoder := r.cfg, r.ffmpeg, r.encoder
	running := r.runner != nil
	// A manual switch is an explicit choice. Stop the auto-switch watcher so
	// it cannot immediately switch back to the focused window a second later.
	watchCancel := r.watchCancel
	r.watchCancel = nil
	r.mu.Unlock()
	if !running {
		return Session{}, errors.New("no capture is running")
	}
	if watchCancel != nil {
		watchCancel()
	}
	if ffmpeg.Path == "" {
		var err error
		ffmpeg, err = r.ensureFFmpeg(ctx, cfg)
		if err != nil {
			return Session{}, err
		}
	}
	return r.switchTo(ctx, cfg, ffmpeg, encoder, &target)
}

// SwitchRejectedError reports a switch that failed before the running capture
// was stopped. The existing stream is still live, so the caller must not tear
// it down.
type SwitchRejectedError struct {
	Err error
}

func (e *SwitchRejectedError) Error() string { return e.Err.Error() }
func (e *SwitchRejectedError) Unwrap() error { return e.Err }

// switchTo stops the current capture and starts one for the new target.
func (r *Recorder) switchTo(ctx context.Context, cfg config.Config, ffmpeg encode.FFmpeg, encoder encode.Encoder, requested *capture.Target) (Session, error) {
	// Build and validate the replacement input before touching the running
	// capture. A switch to an uncapturable target (a monitor with no DXGI
	// output, a window that vanished) must fail here, while the old stream is
	// still alive, instead of taking it down and leaving viewers with nothing.
	prepared, err := r.prepareCapture(ctx, cfg, ffmpeg, encoder, requested)
	if err != nil {
		return Session{}, &SwitchRejectedError{Err: err}
	}
	closePrepared := func() {
		if prepared.frameSource != nil {
			_ = prepared.frameSource.Close()
		}
	}

	// Start the NEW capture first. If it fails, the old capture is still running
	// and we return SwitchRejectedError so the watcher knows not to tear down the stream.
	captureCtx, captureCancel := context.WithCancel(context.WithoutCancel(ctx))
	newSession, err := r.startPrepared(captureCtx, cfg, ffmpeg, encoder, prepared, false)
	if err != nil {
		captureCancel()
		closePrepared()
		return Session{}, &SwitchRejectedError{Err: err}
	}

	// New capture is healthy. Now stop the OLD capture.
	r.mu.Lock()
	old := r.runner
	oldFrameSource := r.frameSource
	oldPump := r.pumpDone
	previous := r.session.LocalRecording

	// Atomically swap to the new capture
	r.runner = nil // will be set by startPrepared via r.mu.Lock inside
	r.frameSource = nil
	r.pumpDone = nil
	r.cancelRun = captureCancel
	r.session = newSession
	r.mu.Unlock()

	// Stop the old frame source before ffmpeg so it sees the end of its input
	// instead of a stalled socket.
	if oldFrameSource != nil {
		if err := oldFrameSource.Close(); err != nil {
			r.logger.Debug("closing the previous window frame source", "error", err)
		}
	}
	if old != nil {
		if err := old.Stop(ctx); err != nil {
			r.logger.Warn("stopping the previous capture", "error", err)
		}
	}
	if oldPump != nil {
		select {
		case <-oldPump:
		case <-ctx.Done():
			return Session{}, ctx.Err()
		}
	}

	if previous != "" {
		r.logger.Info("previous recording finished", "file", previous)
	}
	r.logger.Info("capture target switched", "target", newSession.Description)
	return newSession, nil
}

// sameTarget reports whether two resolutions would capture the same pixels.
func sameTarget(a, b capture.Target) bool {
	if a.Kind != b.Kind {
		return false
	}
	if a.Kind == capture.KindWindow {
		// Window titles change as applications update them, so the handle is
		// the stable identity.
		return a.Window.Handle == b.Window.Handle
	}
	return a.Monitor.Index == b.Monitor.Index
}

// fileURL turns a filesystem path into a file:// URL.
//
// Windows paths need the extra leading slash: C:\Users\me\a.mp4 has to become
// file:///C:/Users/me/a.mp4 for anything to open it.
func fileURL(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String()
}

// ViewerCount returns 0 for the recorder (no live viewers).
func (r *Recorder) ViewerCount() int { return 0 }

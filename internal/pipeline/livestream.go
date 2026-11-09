package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"streamy/internal/capture"
	"streamy/internal/config"
	"streamy/internal/output"
	"streamy/internal/rtc"
	"streamy/internal/signal"
)

// LiveStreamer captures one screen and desktop audio, publishes them over
// WebRTC, and creates an authenticated room link for viewers.
type LiveStreamer struct {
	logger *slog.Logger

	// base is reused by every stream. Its one-time work (finding ffmpeg,
	// probing the encoder, checking Desktop Duplication) costs hundreds of
	// milliseconds, which is exactly the delay the user feels when pressing
	// the hotkey a second time.
	base *Recorder

	mu            sync.Mutex
	recorder      *Recorder
	audio         *rtc.DesktopAudio
	signal        signal.Signaler
	broadcast     *rtc.Broadcaster
	outputs       []output.Output
	reader        *io.PipeReader
	cancel        context.CancelFunc
	done          chan struct{}
	session       Session
	fps           int
	identityToken string
	starting      bool
	startDone     chan struct{}
	writer        *io.PipeWriter
	cfg           config.Config

	// onViewerWatching and onViewerLeft are the app's cues for viewers
	// arriving and leaving. They are set at Start and cleared at Stop.
	onViewerWatching func()
	onViewerLeft     func()
}

// NewLiveStreamer creates the tray app's browser-sharing pipeline.
func NewLiveStreamer(logger *slog.Logger) *LiveStreamer {
	if logger == nil {
		logger = slog.Default()
	}
	return &LiveStreamer{
		logger: logger,
		base:   NewRecorder(Options{Logger: logger, Format: "h264"}),
	}
}

// Start creates the room, starts capture, and returns a browser link.
func (s *LiveStreamer) Start(ctx context.Context, req Request) (Session, error) {
	s.mu.Lock()
	if s.recorder != nil || s.starting {
		s.mu.Unlock()
		return Session{}, errors.New("a live stream is already running")
	}
	s.starting = true
	s.startDone = make(chan struct{})
	s.mu.Unlock()
	defer s.finishStart()

	cfg := req.Config
	relay := cfg.Stream.Relay
	relayEnabled := relay.Enabled
	relayOnly := relayEnabled && relay.Only

	if !relayOnly {
		if cfg.Signaling.ViewerURL == "" {
			return Session{}, errors.New("signaling.viewerURL is empty; configure the deployed viewer URL in config.json")
		}
		if cfg.Signaling.URL == "" {
			return Session{}, errors.New("signaling.url is empty; configure the Worker WebSocket URL in config.json")
		}
		if req.IdentityToken == "" {
			return Session{}, errors.New("sign in with Discord from Streamy settings before starting a public stream")
		}
	}

	// The outputs and capture live beyond the Start call. Detach cancellation
	// from the caller's request context; Stop cancels this context explicitly.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	running := false
	defer func() {
		if !running {
			cancel()
		}
	}()

	// A relay-only stream has no public viewer link, so there is nothing to
	// build or to post to Discord.
	var (
		viewerURL string
		err       error
	)
	if !relayOnly {
		viewerURL, err = signal.LobbyLink(cfg.Signaling.ViewerURL)
		if err != nil {
			return Session{}, err
		}
	}

	// Discord Activity uses a fixed channel as the relay room when
	// ActivityRoom is configured. Otherwise, resolve the streamer's voice
	// channel (falling back to ChannelID). This allows the Activity to work
	// from any channel while all viewers join the same shared room.
	relayRoom := strings.TrimSpace(relay.ChannelID)
	if relayEnabled && strings.TrimSpace(relay.ActivityRoom) != "" {
		// Use the fixed activity room directly - no voice channel resolution needed.
		relayRoom = strings.TrimSpace(relay.ActivityRoom)
		s.logger.Info("using fixed Discord Activity room", "channel", relayRoom)
	} else if relayEnabled && strings.TrimSpace(relay.NotifyURL) != "" && strings.TrimSpace(req.UserID) != "" {
		resolveCtx, resolveCancel := context.WithTimeout(ctx, 10*time.Second)
		resolved, resolveErr := output.NotifyBotResolve(
			resolveCtx, relay.NotifyURL, relay.NotifyKey, relay.ChannelID, req.UserID,
		)
		resolveCancel()
		if resolveErr != nil {
			s.logger.Warn("could not resolve the Discord Activity voice channel", "error", resolveErr)
		} else if resolved.OK && strings.TrimSpace(resolved.ChannelID) != "" {
			relayRoom = strings.TrimSpace(resolved.ChannelID)
			s.logger.Info("resolved Discord Activity channel",
				"channel", relayRoom, "voice", resolved.Voice)
		}
	}

	var room string
	var client signal.Signaler
	var broadcaster *rtc.Broadcaster
	if !relayOnly {
		var token string
		var roomErr error
		room, token, roomErr = signal.NewRoom()
		if roomErr != nil {
			return Session{}, roomErr
		}
		lobbyICEServers := signalingICEServers(cfg)
		client, err = signal.NewWebSocketWithIdentity(
			ctx, cfg.Signaling.URL, room, token, req.IdentityToken, cfg.Stream.MaxViewers, lobbyICEServers,
		)
		if err != nil {
			return Session{}, err
		}
		broadcaster, err = rtc.NewBroadcaster(s.logger)
		if err != nil {
			_ = client.Close()
			return Session{}, err
		}
	} else {
		room = relayRoom
	}

	outputs := make([]output.Output, 0, 2)
	if broadcaster != nil {
		outputs = append(outputs, output.NewWebRtcOutput(broadcaster))
	}

	// Add the WebSocket output for Discord Activity. A relay-only stream has
	// no WebRTC output at all, so this is the only media path.
	if relayEnabled && relayRoom != "" && strings.TrimSpace(relay.URL) != "" && strings.TrimSpace(relay.Token) != "" {
		wsOut, wsErr := output.NewWebSocketOutput(runCtx, output.WebSocketOptions{
			Endpoint: relay.URL,
			Room:     relayRoom,
			Token:    relay.Token,
			Logger:   s.logger,
		})
		if wsErr != nil {
			s.logger.Warn("could not create Discord Activity relay output", "error", wsErr)
		} else {
			outputs = append(outputs, wsOut)
		}
	}
	if len(outputs) == 0 {
		if broadcaster != nil {
			broadcaster.Close()
		}
		if client != nil {
			_ = client.Close()
		}
		return Session{}, errors.New("no stream output is available; enable WebRTC or configure stream.relay")
	}

	closeOutputs := func() {
		for _, out := range outputs {
			_ = out.Close()
		}
	}

	reader, writer := io.Pipe()
	recorder := s.base
	// The writer is deliberately not closed when one ffmpeg capture ends:
	// switching windows restarts ffmpeg around the same pipe, and the video
	// pump must stay alive across that gap. Stop closes it explicitly.
	recorder.SetStdout(writer, false)
	cleanupStart := func() {
		cancel()
		closeOutputs()
		if broadcaster != nil {
			broadcaster.Close()
		}
		if client != nil {
			_ = client.Close()
		}
		_ = reader.Close()
		_ = writer.Close()
	}

	// Desktop audio is encoded once to Opus and copied to every output. Today
	// there is one output, WebRTC, but keeping the fan-out makes the media path
	// independent of the transport.
	audioSink := func(packet *rtp.Packet) error {
		now := time.Now()
		var firstErr error
		for _, out := range outputs {
			if err := out.WriteAudio(packet, now); err != nil {
				s.logger.Warn("sending audio to an output", "output", out.Name(), "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr
	}

	// Audio capture waits for WASAPI and for the first encoded packet, which is
	// most of a second. Recording the screen is independent of it, so both are
	// brought up at the same time instead of one after the other.
	var audioCapture *rtc.DesktopAudio
	var audioErr error
	var audioDone chan struct{}
	if cfg.Capture.DesktopAudio {
		ffmpeg, err := recorder.ensureFFmpeg(ctx, cfg)
		if err != nil {
			cleanupStart()
			return Session{}, fmt.Errorf("prepare desktop audio: %w", err)
		}
		audioDone = make(chan struct{})
		go func() {
			defer close(audioDone)
			audioCapture, audioErr = rtc.StartDesktopAudio(
				runCtx, ffmpeg.Path, cfg.Capture.ExcludeDiscordAudio, s.logger, audioSink,
			)
		}()
	}

	captureSession, captureErr := recorder.Start(ctx, req)
	if audioDone != nil {
		select {
		case <-audioDone:
		case <-ctx.Done():
			cancel()
			<-audioDone
		}
	}
	if captureErr != nil {
		if audioCapture != nil {
			if closeErr := audioCapture.Close(); closeErr != nil {
				s.logger.Warn("closing desktop-audio capture after screen startup failure", "error", closeErr)
			}
		}
		cleanupStart()
		return Session{}, fmt.Errorf("start screen capture: %w", captureErr)
	}
	if audioErr != nil {
		if stopErr := recorder.Stop(ctx); stopErr != nil {
			s.logger.Warn("stopping screen capture after audio startup failure", "error", stopErr)
		}
		cleanupStart()
		return Session{}, fmt.Errorf("start desktop audio: %w", audioErr)
	}
	session := Session{
		ViewerURL:   viewerURL,
		Label:       "Sharing",
		Description: fmt.Sprintf("%s; viewer link copied to clipboard", captureSession.Description),
		Target:      captureSession.Target,
	}
	if relayOnly {
		session.Label = "Discord Activity"
		session.Description = fmt.Sprintf("%s; streaming to Discord Activity", captureSession.Description)
	}
	done := make(chan struct{})

	s.mu.Lock()
	s.recorder = recorder
	s.audio = audioCapture
	s.signal = client
	s.broadcast = broadcaster
	s.outputs = outputs
	s.reader = reader
	s.cancel = cancel
	s.done = done
	s.session = session
	s.fps = cfg.Encoding.FPS
	s.identityToken = req.IdentityToken
	s.writer = writer
	s.cfg = cfg
	s.onViewerWatching = req.OnViewerWatching
	s.onViewerLeft = req.OnViewerLeft
	s.mu.Unlock()

	go s.forwardVideo(runCtx, reader)
	if client != nil {
		go s.handleSignals(runCtx, client, cfg, signalingICEServers(cfg))
	}

	// Notify the Discord bot. The Activity itself is launched by the /watch
	// command; this message is only the plain viewer link in the configured
	// text channel. The resolved voice channel is not text-based, so it must
	// not be used as the notification target.
	discord := cfg.Stream.Discord
	notifyURL := discord.NotifyURL
	notifyKey := discord.NotifyKey
	notifyChannelID := discord.ChannelID
	if relayEnabled && relay.NotifyURL != "" {
		notifyURL = relay.NotifyURL
		notifyKey = relay.NotifyKey
		notifyChannelID = relay.ChannelID
	}
	if !relayOnly && notifyURL != "" && strings.TrimSpace(notifyChannelID) != "" {
		go func() {
			notifyCtx, notifyCancel := context.WithTimeout(runCtx, 15*time.Second)
			defer notifyCancel()
			err := output.NotifyBotStart(notifyCtx, notifyURL, notifyKey, output.BotNotice{
				ChannelID: notifyChannelID,
				Streamer:  req.UserName,
				URL:       viewerURL,
			})
			if err != nil {
				s.logger.Warn("could not notify the Discord bot", "error", err)
			} else {
				s.logger.Info("notified the Discord bot", "channel", notifyChannelID)
			}
		}()
	}
	if relayEnabled {
		s.logger.Info("live room ready", "room", room, "activity_room", relayRoom, "maxViewers", cfg.Stream.MaxViewers)
	} else {
		s.logger.Info("live room ready", "room", room, "maxViewers", cfg.Stream.MaxViewers)
	}
	running = true
	return session, nil
}

func (s *LiveStreamer) finishStart() {
	s.mu.Lock()
	s.starting = false
	if s.startDone != nil {
		close(s.startDone)
		s.startDone = nil
	}
	s.mu.Unlock()
}

func (s *LiveStreamer) forwardVideo(ctx context.Context, input io.Reader) {
	defer close(s.done)
	reader := rtc.NewAccessUnitReader(input)
	duration := time.Second / time.Duration(s.sessionFPS())
	var lastBandwidthLog time.Time
	var bytesSinceLog int64

	// Stall detection: if no access unit arrives within 5s of capture start,
	// log ffmpeg stderr tail so the black-screen failure mode is visible.
	firstFrameDeadline := time.NewTimer(5 * time.Second)
	defer firstFrameDeadline.Stop()
	firstFrameReceived := false

	for {
		select {
		case <-ctx.Done():
			_ = reader.Drain()
			return
		case <-firstFrameDeadline.C:
			if !firstFrameReceived {
				s.mu.Lock()
				recorder := s.recorder
				s.mu.Unlock()
				if recorder != nil {
					s.logger.Error("no video frames received within 5s of capture start; ffmpeg stderr tail",
						"stderr", recorder.FFmpegStderr())
				}
			}
		default:
		}
		accessUnit, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if ctx.Err() != nil {
				// Stop closes the pipe under a pump that may be blocked in a
				// read. That is the end of the session, not a malformed stream:
				// logging it as a parse failure only makes shutdown look broken.
				s.logger.Debug("video pump stopped during shutdown", "error", err)
				return
			}
			s.logger.Error("could not parse ffmpeg H.264 output", "error", err)
			return
		}

		if !firstFrameReceived {
			firstFrameReceived = true
			firstFrameDeadline.Stop()
			s.logger.Info("first video frame received", "bytes", len(accessUnit))
		}

		s.mu.Lock()
		outputs := append([]output.Output(nil), s.outputs...)
		broadcaster := s.broadcast
		s.mu.Unlock()
		if len(outputs) == 0 {
			continue
		}
		now := time.Now()
		sent := false
		for _, out := range outputs {
			if err := out.WriteVideo(accessUnit, now, duration); err != nil {
				s.logger.Warn("sending video to an output failed", "output", out.Name(), "error", err)
				continue
			}
			sent = true
		}
		if !sent {
			continue
		}
		if broadcaster != nil {
			bytesSinceLog += int64(len(accessUnit) * broadcaster.ViewerCount())
		}
		if lastBandwidthLog.IsZero() {
			lastBandwidthLog = time.Now()
		}
		if elapsed := time.Since(lastBandwidthLog); elapsed >= 10*time.Second {
			logArgs := []any{"estimated_kbps", float64(bytesSinceLog) * 8 / elapsed.Seconds() / 1000}
			if broadcaster != nil {
				logArgs = append(logArgs, "viewers", broadcaster.ViewerCount())
			}
			s.logger.Info("video output bandwidth", logArgs...)
			lastBandwidthLog = time.Now()
			bytesSinceLog = 0
		}
	}
}

func (s *LiveStreamer) sessionFPS() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fps < 1 {
		return 30
	}
	return s.fps
}

func (s *LiveStreamer) handleSignals(ctx context.Context, client signal.Signaler, cfg config.Config, lobbyICEServers []signal.ICEServer) {
	room, token := client.Room(), client.Token()
	backoff := time.Second
	for {
		if !s.consumeSignals(ctx, client, cfg) || ctx.Err() != nil {
			_ = client.Close()
			return
		}
		if err := client.Err(); err != nil {
			s.logger.Warn("signaling disconnected; attempting to reconnect", "error", err)
		}
		_ = client.Close()
		s.mu.Lock()
		broadcaster := s.broadcast
		s.mu.Unlock()
		if broadcaster != nil {
			broadcaster.Close()
			broadcaster.RequestKeyframe("signaling-reconnect")
		}

		for {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			reconnectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			reconnected, err := signal.NewWebSocketWithIdentity(
				reconnectCtx, cfg.Signaling.URL, room, token, s.identityToken, cfg.Stream.MaxViewers, lobbyICEServers,
			)
			cancel()
			if err != nil {
				s.logger.Warn("signaling reconnect failed", "retry_in", backoff, "error", err)
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			client = reconnected
			s.mu.Lock()
			if ctx.Err() != nil {
				s.mu.Unlock()
				_ = reconnected.Close()
				return
			}
			s.signal = reconnected
			s.mu.Unlock()
			s.logger.Info("signaling connection restored", "room", room)
			backoff = time.Second
			break
		}
	}
}

func (s *LiveStreamer) consumeSignals(ctx context.Context, client signal.Signaler, cfg config.Config) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case message, ok := <-client.Messages():
			if !ok {
				if err := client.Err(); err != nil {
					s.logger.Error("signaling connection closed", "error", err)
				}
				return true
			}
			switch message.Type {
			case "welcome":
				s.logger.Info("connected to signaling room",
					"peer_id", message.PeerID,
					"viewers", message.ViewerCount,
					"viewer_limit", message.MaxViewers,
				)
			case "peer-joined":
				if message.Role == signal.RoleViewer {
					go s.startViewer(ctx, client, cfg, message.PeerID)
				}
			case "peer-left":
				if message.Role == signal.RoleViewer {
					s.mu.Lock()
					broadcaster := s.broadcast
					s.mu.Unlock()
					if broadcaster != nil {
						// Only viewers that actually saw video deserve a
						// goodbye cue; a page that opened and closed without
						// negotiating never chimed in the first place.
						watching := broadcaster.Peer(message.PeerID).WasConnected()
						broadcaster.RemovePeer(message.PeerID)
						if watching {
							s.notifyViewerLeft()
						}
					}
					s.logger.Info("viewer left", "peer_id", message.PeerID, "reason", message.Reason)
				}
			case "answer":
				s.mu.Lock()
				broadcaster := s.broadcast
				s.mu.Unlock()
				if broadcaster != nil {
					if peer := broadcaster.Peer(message.From); peer != nil {
						if err := peer.SetAnswer(message.SDP); err != nil {
							s.logger.Error("applying viewer answer", "peer_id", message.From, "error", err)
						}
					} else {
						s.logger.Debug("ignoring answer from an unknown viewer", "peer_id", message.From)
					}
				}
			case "candidate":
				s.logger.Debug("ignoring trickle candidate; phase 4 uses complete SDP", "peer_id", message.From)
			case "error":
				s.logger.Error("signaling server error", "code", message.Code, "message", message.ErrorMessage)
				if message.Fatal == nil || *message.Fatal {
					return false
				}
			case "pong":
			default:
				s.logger.Warn("ignoring unknown signaling message", "type", message.Type)
			}
		}
	}
}

func (s *LiveStreamer) startViewer(ctx context.Context, client signal.Signaler, cfg config.Config, peerID string) {
	s.mu.Lock()
	broadcaster := s.broadcast
	if broadcaster == nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	if broadcaster.Peer(peerID) != nil {
		return
	}
	peer, err := broadcaster.AddPeer(peerID, iceServers(cfg))
	if err != nil {
		s.logger.Warn("could not add WebRTC peer for viewer", "peer_id", peerID, "error", err)
		return
	}
	offer, err := peer.CreateOffer(ctx)
	if err != nil {
		broadcaster.RemovePeer(peerID)
		s.logger.Error("create WebRTC offer", "peer_id", peerID, "error", err)
		return
	}
	if ctx.Err() != nil {
		broadcaster.RemovePeer(peerID)
		return
	}
	if err := client.Send(ctx, signal.Message{Type: "offer", To: peerID, SDP: offer}); err != nil {
		broadcaster.RemovePeer(peerID)
		s.logger.Error("send WebRTC offer", "peer_id", peerID, "error", err)
		return
	}

	s.logger.Info("sent WebRTC offer", "peer_id", peerID)
	go s.watchViewer(ctx, peerID, peer)
	go func() {
		select {
		case err := <-peer.Failure():
			if errors.Is(err, context.Canceled) {
				return
			}
			s.logger.Warn("viewer WebRTC connection ended", "peer_id", peerID, "error", err)
			broadcaster.RemovePeer(peerID)
		case <-ctx.Done():
		}
	}()
}

// watchViewer waits for one viewer's media path to come up. Signaling says a
// page connected; this says a person is actually watching.
func (s *LiveStreamer) watchViewer(ctx context.Context, peerID string, peer *rtc.Peer) {
	select {
	case <-peer.Connected():
		s.mu.Lock()
		notify := s.onViewerWatching
		broadcaster := s.broadcast
		s.mu.Unlock()
		if notify == nil || broadcaster == nil || broadcaster.Peer(peerID) != peer {
			// The viewer left before the connection event was handled, so
			// the departure cue already told the story.
			return
		}
		s.logger.Info("viewer is watching", "peer_id", peerID)
		notify()
	case <-ctx.Done():
	}
}

// notifyViewerLeft runs the viewer-left cue away from the signaling read loop.
func (s *LiveStreamer) notifyViewerLeft() {
	s.mu.Lock()
	notify := s.onViewerLeft
	s.mu.Unlock()
	if notify != nil {
		notify()
	}
}

func iceServers(cfg config.Config) []webrtc.ICEServer {
	servers := make([]webrtc.ICEServer, 0, len(cfg.Stream.STUNServers)+len(cfg.Stream.TURNServers))
	for _, server := range cfg.Stream.STUNServers {
		servers = append(servers, webrtc.ICEServer{URLs: []string{server}})
	}
	for _, server := range cfg.Stream.TURNServers {
		servers = append(servers, webrtc.ICEServer{
			URLs:       server.URLs,
			Username:   server.Username,
			Credential: server.Credential,
		})
	}
	return servers
}

func signalingICEServers(cfg config.Config) []signal.ICEServer {
	servers := make([]signal.ICEServer, 0, len(cfg.Stream.STUNServers)+len(cfg.Stream.TURNServers))
	for _, server := range cfg.Stream.STUNServers {
		servers = append(servers, signal.ICEServer{URLs: []string{server}})
	}
	for _, server := range cfg.Stream.TURNServers {
		servers = append(servers, signal.ICEServer{
			URLs: server.URLs, Username: server.Username, Credential: server.Credential,
		})
	}
	return servers
}

// SwitchTarget retargets the capture without closing the room or the outputs.
// Viewers stay connected; ffmpeg restarts its input and a keyframe is requested
// so the new window appears immediately.
func (s *LiveStreamer) SwitchTarget(ctx context.Context, target capture.Target) (Session, error) {
	s.mu.Lock()
	recorder := s.recorder
	broadcaster := s.broadcast
	session := s.session
	s.mu.Unlock()
	if recorder == nil {
		return Session{}, errors.New("no live stream is running")
	}
	if _, err := recorder.SwitchTarget(ctx, target); err != nil {
		return Session{}, err
	}
	if broadcaster != nil {
		broadcaster.RequestKeyframe("target-switch")
	}
	s.logger.Info("stream target switched", "target", target.Describe())
	return session, nil
}

// Stop closes signaling and capture, and waits for ffmpeg's output pump.
func (s *LiveStreamer) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.starting {
		startDone := s.startDone
		s.mu.Unlock()
		select {
		case <-startDone:
			return s.Stop(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	recorder, audio, client, outputs := s.recorder, s.audio, s.signal, s.outputs
	cancel, reader, done, writer := s.cancel, s.reader, s.done, s.writer
	if cancel != nil {
		cancel()
	}
	s.recorder, s.audio, s.signal, s.broadcast, s.outputs = nil, nil, nil, nil, nil
	s.cancel, s.reader, s.done, s.session, s.fps = nil, nil, nil, Session{}, 0
	s.writer = nil
	s.onViewerWatching, s.onViewerLeft = nil, nil
	s.mu.Unlock()
	if recorder == nil {
		return nil
	}

	if audio != nil {
		if err := audio.Close(); err != nil {
			s.logger.Warn("closing desktop-audio capture", "error", err)
		}
	}
	if client != nil {
		if err := client.Close(); err != nil {
			s.logger.Warn("closing signaling connection", "error", err)
		}
	}
	err := recorder.Stop(ctx)
	// Close writer so forwardVideo sees io.EOF and exits promptly without
	// hanging until ctx.Done().
	if writer != nil {
		_ = writer.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			err = errors.Join(err, ctx.Err())
			// Context deadline fired: force-unblock the pump by closing the
			// reader so it returns io.ErrClosedPipe.
			if reader != nil {
				_ = reader.Close()
			}
		}
	}
	if reader != nil {
		_ = reader.Close()
	}
	for _, out := range outputs {
		if closeErr := out.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s output: %w", out.Name(), closeErr))
		}
	}
	return err
}

// ViewerCount returns the current number of connected viewers.
func (s *LiveStreamer) ViewerCount() int {
	s.mu.Lock()
	broadcaster := s.broadcast
	s.mu.Unlock()
	if broadcaster == nil {
		return 0
	}
	return broadcaster.ViewerCount()
}

var _ Streamer = (*LiveStreamer)(nil)

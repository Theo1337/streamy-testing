package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Broadcaster publishes shared H.264 video and Opus audio tracks to any number
// of peers. Each peer has its own PeerConnection, while the encoded source is
// shared across all viewers.
type Broadcaster struct {
	track  *webrtc.TrackLocalStaticSample
	audio  *webrtc.TrackLocalStaticRTP
	logger Logger

	mu              sync.RWMutex
	peers           map[string]*Peer
	waitingKeyframe bool
	sps             []byte
	pps             []byte
}

// Peer represents one viewer connection to a Broadcaster.
type Peer struct {
	id          string
	pc          *webrtc.PeerConnection
	source      *Broadcaster
	failure     chan error
	failOnce    sync.Once
	close       sync.Once
	connectOnce sync.Once
	connected   chan struct{}
	logger      Logger
}

// NewBroadcaster creates the shared H.264 track.
func NewBroadcaster(logger Logger) (*Broadcaster, error) {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}},
		},
		"video",
		"streamy",
	)
	if err != nil {
		return nil, fmt.Errorf("create shared H.264 video track: %w", err)
	}
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		"audio",
		"streamy",
	)
	if err != nil {
		return nil, fmt.Errorf("create shared Opus audio track: %w", err)
	}
	return &Broadcaster{
		track:           track,
		audio:           audioTrack,
		logger:          logger,
		peers:           make(map[string]*Peer),
		waitingKeyframe: true,
	}, nil
}

// AddPeer creates a PeerConnection for one viewer and adds the shared video
// track to it.
func (b *Broadcaster) AddPeer(id string, iceServers []webrtc.ICEServer) (*Peer, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("viewer peer ID must not be empty")
	}
	b.mu.Lock()
	if _, exists := b.peers[id]; exists {
		b.mu.Unlock()
		return nil, fmt.Errorf("viewer %q already has a peer connection", id)
	}
	b.waitingKeyframe = true

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("create WebRTC peer connection for %s: %w", id, err)
	}
	rtpSender, err := pc.AddTrack(b.track)
	if err != nil {
		_ = pc.Close()
		b.mu.Unlock()
		return nil, fmt.Errorf("add shared video track for %s: %w", id, err)
	}
	audioSender, err := pc.AddTrack(b.audio)
	if err != nil {
		_ = pc.Close()
		b.mu.Unlock()
		return nil, fmt.Errorf("add shared audio track for %s: %w", id, err)
	}
	peer := &Peer{
		id:        id,
		pc:        pc,
		source:    b,
		failure:   make(chan error, 1),
		connected: make(chan struct{}),
		logger:    b.logger,
	}
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if peer.logger != nil {
			peer.logger.Info("WebRTC ICE state changed", "peer_id", id, "state", state.String())
		}
		if state == webrtc.ICEConnectionStateFailed {
			peer.reportFailure(errors.New("WebRTC ICE connection failed; check firewall and NAT settings"))
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if peer.logger != nil {
			peer.logger.Info("WebRTC connection state changed", "peer_id", id, "state", state.String())
		}
		if state == webrtc.PeerConnectionStateConnected {
			peer.connectOnce.Do(func() { close(peer.connected) })
		}
		if state == webrtc.PeerConnectionStateFailed {
			peer.reportFailure(errors.New("WebRTC peer connection failed"))
		}
	})
	b.peers[id] = peer
	b.mu.Unlock()
	go peer.readRTCP(rtpSender, true)
	go peer.readRTCP(audioSender, false)
	return peer, nil
}

// WriteAudioRTP broadcasts an encoded Opus packet to every connected viewer.
func (b *Broadcaster) WriteAudioRTP(packet *rtp.Packet) error {
	if packet == nil || len(packet.Payload) == 0 {
		return errors.New("cannot send an empty Opus RTP packet")
	}
	if err := b.audio.WriteRTP(packet); err != nil {
		return fmt.Errorf("broadcast Opus RTP packet: %w", err)
	}
	return nil
}

// Peer looks up a viewer's current PeerConnection.
func (b *Broadcaster) Peer(id string) *Peer {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.peers[id]
}

// RemovePeer closes and removes a viewer's peer connection.
func (b *Broadcaster) RemovePeer(id string) {
	b.mu.Lock()
	peer := b.peers[id]
	delete(b.peers, id)
	b.mu.Unlock()
	if peer != nil {
		_ = peer.Close()
	}
}

// Close removes every viewer peer and releases the shared track bindings.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	peers := make([]*Peer, 0, len(b.peers))
	for id, peer := range b.peers {
		delete(b.peers, id)
		peers = append(peers, peer)
	}
	b.mu.Unlock()
	for _, peer := range peers {
		_ = peer.Close()
	}
}

// ViewerCount reports the number of connected or negotiating viewer peers.
func (b *Broadcaster) ViewerCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.peers)
}

// WriteSample broadcasts one complete H.264 access unit. After a viewer joins
// or requests an intra frame, delta frames are held until the encoder's next
// periodic IDR, which is why the default GOP is one second.
func (b *Broadcaster) WriteSample(data []byte, duration time.Duration) error {
	if len(data) == 0 {
		return errors.New("cannot send an empty H.264 access unit")
	}
	if duration <= 0 {
		return errors.New("H.264 sample duration must be positive")
	}

	b.mu.Lock()
	hasSPS, hasPPS, hasIDR := inspectNALs(data)
	if hasSPS {
		b.sps = extractNALs(data, 7)
	}
	if hasPPS {
		b.pps = extractNALs(data, 8)
	}
	if b.waitingKeyframe && !hasIDR {
		b.mu.Unlock()
		return nil
	}
	if b.waitingKeyframe && hasIDR {
		if !hasSPS {
			data = append(append([]byte(nil), b.sps...), data...)
		}
		if !hasPPS {
			data = insertBeforeFirstIDR(data, b.pps)
		}
		b.waitingKeyframe = false
	}
	b.mu.Unlock()

	if err := b.track.WriteSample(media.Sample{Data: data, Duration: duration}); err != nil {
		return fmt.Errorf("broadcast H.264 sample: %w", err)
	}
	return nil
}

// RequestKeyframe marks the shared source as needing an IDR for clean
// resynchronization at the next encoder keyframe.
func (b *Broadcaster) RequestKeyframe(peerID string) {
	b.requestKeyframe(peerID)
}

func (b *Broadcaster) requestKeyframe(peerID string) {
	b.mu.Lock()
	alreadyWaiting := b.waitingKeyframe
	b.waitingKeyframe = true
	b.mu.Unlock()
	if !alreadyWaiting && b.logger != nil {
		b.logger.Info("viewer requested keyframe; resynchronizing at next encoder IDR", "peer_id", peerID)
	}
}

// CreateOffer gathers ICE candidates so the complete SDP can be relayed in one
// message before the browser answers.
func (p *Peer) CreateOffer(ctx context.Context) (string, error) {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("create WebRTC offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("set local WebRTC offer: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return "", fmt.Errorf("gather ICE candidates: %w", ctx.Err())
	}
	local := p.pc.LocalDescription()
	if local == nil || strings.TrimSpace(local.SDP) == "" {
		return "", errors.New("WebRTC produced an empty offer")
	}
	return local.SDP, nil
}

// SetAnswer applies the browser's raw SDP or JSON SessionDescription.
func (p *Peer) SetAnswer(answer string) error {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return errors.New("the viewer answer is empty")
	}
	var description webrtc.SessionDescription
	if strings.HasPrefix(answer, "{") {
		if err := json.Unmarshal([]byte(answer), &description); err != nil {
			return fmt.Errorf("parse viewer answer JSON: %w", err)
		}
	} else {
		description = webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}
	}
	if description.Type != webrtc.SDPTypeAnswer {
		return fmt.Errorf("the viewer description has type %q, want answer", description.Type.String())
	}
	description.SDP = strings.TrimSpace(description.SDP) + "\r\n"
	if err := p.pc.SetRemoteDescription(description); err != nil {
		return fmt.Errorf("apply viewer answer: %w", err)
	}
	return nil
}

// Failure is signalled when this viewer's ICE or peer connection fails.
func (p *Peer) Failure() <-chan error { return p.failure }

// Connected is closed once this viewer's WebRTC connection is established.
// Waiting on it tells the host that video is actually reaching the viewer,
// rather than that the viewer merely opened the page.
func (p *Peer) Connected() <-chan struct{} { return p.connected }

// WasConnected reports whether the viewer's connection was ever established.
// It is safe to call on a nil peer.
func (p *Peer) WasConnected() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.connected:
		return true
	default:
		return false
	}
}

// Close releases the peer connection and its RTCP reader.
func (p *Peer) Close() error {
	var err error
	p.close.Do(func() { err = p.pc.Close() })
	return err
}

func (p *Peer) reportFailure(err error) {
	p.failOnce.Do(func() {
		p.failure <- err
	})
}

func (p *Peer) readRTCP(sender *webrtc.RTPSender, video bool) {
	buf := make([]byte, 1500)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			if p.logger != nil {
				p.logger.Debug("could not parse viewer RTCP feedback", "peer_id", p.id, "error", err)
			}
			continue
		}
		for _, packet := range packets {
			if video {
				switch packet.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					p.source.requestKeyframe(p.id)
				}
			}
		}
	}
}

func inspectNALs(data []byte) (hasSPS, hasPPS, hasIDR bool) {
	for _, nal := range splitNALs(data) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case 5:
			hasIDR = true
		case 7:
			hasSPS = true
		case 8:
			hasPPS = true
		}
	}
	return
}

func extractNALs(data []byte, wanted byte) []byte {
	var result []byte
	for _, nal := range splitNALs(data) {
		if len(nal) > 0 && nal[0]&0x1f == wanted {
			result = append(result, 0, 0, 0, 1)
			result = append(result, nal...)
		}
	}
	return result
}

func insertBeforeFirstIDR(data, prefix []byte) []byte {
	if len(prefix) == 0 {
		return data
	}
	for _, part := range splitNALOffsets(data) {
		if len(part.nal) > 0 && part.nal[0]&0x1f == 5 {
			result := make([]byte, 0, len(data)+len(prefix))
			result = append(result, data[:part.start]...)
			result = append(result, prefix...)
			result = append(result, data[part.start:]...)
			return result
		}
	}
	return data
}

type nalPart struct {
	start int
	nal   []byte
}

func splitNALs(data []byte) [][]byte {
	parts := splitNALOffsets(data)
	result := make([][]byte, 0, len(parts))
	for _, part := range parts {
		result = append(result, part.nal)
	}
	return result
}

func splitNALOffsets(data []byte) []nalPart {
	var starts []int
	var startLengths []int
	for i := 0; i+2 < len(data); {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			starts = append(starts, i)
			startLengths = append(startLengths, 3)
			i += 3
		} else if i+3 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			starts = append(starts, i)
			startLengths = append(startLengths, 4)
			i += 4
		} else {
			i++
		}
	}
	parts := make([]nalPart, 0, len(starts))
	for index, start := range starts {
		end := len(data)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		nalStart := start + startLengths[index]
		if nalStart <= end {
			parts = append(parts, nalPart{start: start, nal: data[nalStart:end]})
		}
	}
	return parts
}

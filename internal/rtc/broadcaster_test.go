package rtc

import (
	"context"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestBroadcasterSharesTrackWithMultipleViewers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	broadcaster, err := NewBroadcaster(nil)
	if err != nil {
		t.Fatalf("NewBroadcaster: %v", err)
	}
	defer broadcaster.Close()

	viewers := make([]*webrtc.PeerConnection, 2)
	type receivedTrack struct {
		viewer string
		kind   webrtc.RTPCodecType
	}
	received := make(chan receivedTrack, len(viewers)*2)
	connected := make(chan string, len(viewers))
	for i := range viewers {
		id := string(rune('a' + i))
		viewer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatalf("create viewer %s: %v", id, err)
		}
		viewers[i] = viewer
		t.Cleanup(func() { _ = viewer.Close() })
		viewer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			if state == webrtc.PeerConnectionStateConnected {
				connected <- id
			}
		})
		if _, err := viewer.AddTransceiverFromKind(
			webrtc.RTPCodecTypeVideo,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
		); err != nil {
			t.Fatalf("add viewer %s video transceiver: %v", id, err)
		}
		if _, err := viewer.AddTransceiverFromKind(
			webrtc.RTPCodecTypeAudio,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
		); err != nil {
			t.Fatalf("add viewer %s audio transceiver: %v", id, err)
		}
		viewer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			received <- receivedTrack{viewer: id, kind: track.Kind()}
		})

		hostPeer, err := broadcaster.AddPeer(id, nil)
		if err != nil {
			t.Fatalf("AddPeer %s: %v", id, err)
		}
		offer, err := hostPeer.CreateOffer(ctx)
		if err != nil {
			t.Fatalf("CreateOffer %s: %v", id, err)
		}
		if err := viewer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
			t.Fatalf("viewer %s applies offer: %v", id, err)
		}
		answer, err := viewer.CreateAnswer(nil)
		if err != nil {
			t.Fatalf("viewer %s creates answer: %v", id, err)
		}
		gathered := webrtc.GatheringCompletePromise(viewer)
		if err := viewer.SetLocalDescription(answer); err != nil {
			t.Fatalf("viewer %s sets answer: %v", id, err)
		}
		select {
		case <-gathered:
		case <-ctx.Done():
			t.Fatal("viewer ICE gathering timed out")
		}
		if err := hostPeer.SetAnswer(viewer.LocalDescription().SDP); err != nil {
			t.Fatalf("host applies viewer %s answer: %v", id, err)
		}
	}

	connectedViewers := make(map[string]bool, len(viewers))
	for len(connectedViewers) < len(viewers) {
		select {
		case id := <-connected:
			connectedViewers[id] = true
		case <-ctx.Done():
			t.Fatalf("only viewers %v established ICE connections", connectedViewers)
		}
	}

	if got := broadcaster.ViewerCount(); got != len(viewers) {
		t.Fatalf("ViewerCount = %d, want %d", got, len(viewers))
	}

	// Each viewer's media path is reported as connected, which is what the
	// host uses to play the "someone is watching" cue.
	for id := range connectedViewers {
		peer := broadcaster.Peer(id)
		if peer == nil {
			t.Fatalf("Peer(%q) is nil after connecting", id)
		}
		select {
		case <-peer.Connected():
		case <-ctx.Done():
			t.Fatalf("peer %q never reported a connected media path", id)
		}
		if !peer.WasConnected() {
			t.Fatalf("WasConnected(%q) = false after connecting", id)
		}
	}

	// The first delta frame is dropped after peers join. The next IDR is
	// delivered over the shared track to both PeerConnections.
	if err := broadcaster.WriteSample([]byte{0, 0, 0, 1, 0x41, 0x80}, time.Second/30); err != nil {
		t.Fatalf("WriteSample delta: %v", err)
	}
	idr := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f,
		0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80,
		0, 0, 0, 1, 0x65, 0x80,
	}
	for sequence := range 5 {
		if err := broadcaster.WriteSample(idr, time.Second/30); err != nil {
			t.Fatalf("WriteSample IDR: %v", err)
		}
		if err := broadcaster.WriteAudioRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: uint16(sequence + 1)},
			Payload: []byte{0xf8, 0xff, 0xfe},
		}); err != nil {
			t.Fatalf("WriteAudioRTP: %v", err)
		}
	}

	seen := map[string]map[webrtc.RTPCodecType]bool{}
	for len(seen) < len(viewers) || !allTracksSeen(seen, len(viewers)) {
		select {
		case track := <-received:
			if seen[track.viewer] == nil {
				seen[track.viewer] = make(map[webrtc.RTPCodecType]bool)
			}
			seen[track.viewer][track.kind] = true
		case <-ctx.Done():
			t.Fatalf("viewers did not receive both shared tracks: %v", seen)
		}
	}
}

func allTracksSeen(seen map[string]map[webrtc.RTPCodecType]bool, viewers int) bool {
	if len(seen) < viewers {
		return false
	}
	for _, tracks := range seen {
		if !tracks[webrtc.RTPCodecTypeVideo] || !tracks[webrtc.RTPCodecTypeAudio] {
			return false
		}
	}
	return true
}

func TestPeerWasConnectedBeforeNegotiation(t *testing.T) {
	broadcaster, err := NewBroadcaster(nil)
	if err != nil {
		t.Fatalf("NewBroadcaster: %v", err)
	}
	defer broadcaster.Close()

	peer, err := broadcaster.AddPeer("pending", nil)
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if peer.WasConnected() {
		t.Fatal("a fresh peer reported a connected media path")
	}
}

func TestPLIRequestsKeyframeAndWaitsForNextIDR(t *testing.T) {
	broadcaster, err := NewBroadcaster(nil)
	if err != nil {
		t.Fatalf("NewBroadcaster: %v", err)
	}
	defer broadcaster.Close()
	broadcaster.mu.Lock()
	broadcaster.waitingKeyframe = false
	broadcaster.mu.Unlock()

	broadcaster.RequestKeyframe("viewer")
	broadcaster.mu.RLock()
	waiting := broadcaster.waitingKeyframe
	broadcaster.mu.RUnlock()
	if !waiting {
		t.Fatal("PLI did not mark the broadcaster as waiting for an IDR")
	}

	// Interframes do not clear the PLI request; only an IDR does.
	if err := broadcaster.WriteSample([]byte{0, 0, 0, 1, 0x41, 0x80}, time.Second/30); err != nil {
		t.Fatalf("WriteSample interframe: %v", err)
	}
	broadcaster.mu.RLock()
	waiting = broadcaster.waitingKeyframe
	broadcaster.mu.RUnlock()
	if !waiting {
		t.Fatal("an interframe incorrectly cleared the keyframe request")
	}
	if err := broadcaster.WriteSample([]byte{0, 0, 0, 1, 0x65, 0x80}, time.Second/30); err != nil {
		t.Fatalf("WriteSample IDR: %v", err)
	}
	broadcaster.mu.RLock()
	waiting = broadcaster.waitingKeyframe
	broadcaster.mu.RUnlock()
	if waiting {
		t.Fatal("IDR did not clear the keyframe request")
	}
}

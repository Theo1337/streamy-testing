package output_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"

	"streamy/internal/activity/protocol"
	"streamy/internal/activity/relay"
	"streamy/internal/output"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}

func readMedia(t *testing.T, conn *websocket.Conn) protocol.MediaFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", messageType)
	}
	frame, err := protocol.ParseMediaFrame(data)
	if err != nil {
		t.Fatalf("parse media frame: %v", err)
	}
	return frame
}

// TestWebSocketOutputPublishesToRelay is the desktop-publisher half of the
// Activity path: output.WebSocketOutput connects to a real relay with the
// configured token, and a viewer receives the exact video and audio frames the
// capture pipeline wrote.
func TestWebSocketOutputPublishesToRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relayAddr := freeLoopbackAddr(t)
	relaySrv := relay.NewServer(testLogger(), "publish-token", 5)
	go func() { _ = relaySrv.Run(ctx, relayAddr) }()

	deadline := time.Now().Add(5 * time.Second)
	for relaySrv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if relaySrv.Addr() == "" {
		t.Fatal("relay did not start")
	}

	viewer, _, err := websocket.DefaultDialer.Dial("ws://"+relayAddr+"/v1/watch/room-1", nil)
	if err != nil {
		t.Fatalf("dial viewer: %v", err)
	}
	defer viewer.Close()
	if err := viewer.WriteJSON(map[string]any{
		"v": protocol.ProtocolVersion, "type": "hello", "role": "viewer",
	}); err != nil {
		t.Fatalf("viewer hello: %v", err)
	}
	var ready struct {
		Type string `json:"type"`
	}
	_ = viewer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := viewer.ReadJSON(&ready); err != nil {
		t.Fatalf("viewer ready: %v", err)
	}
	if ready.Type != "ready" {
		t.Fatalf("viewer handshake = %q, want ready", ready.Type)
	}

	publisher, err := output.NewWebSocketOutput(ctx, output.WebSocketOptions{
		Endpoint: "ws://" + relayAddr,
		Room:     "room-1",
		Token:    "publish-token",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	defer publisher.Close()

	keyframe := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xE0, 0x1F,
		0, 0, 0, 1, 0x68, 0xCE, 0x3C, 0x80,
		0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00,
	}
	if err := publisher.WriteVideo(keyframe, time.Now(), time.Second/30); err != nil {
		t.Fatalf("write video: %v", err)
	}

	video := readMedia(t, viewer)
	if video.Kind != protocol.MediaVideo {
		t.Fatalf("frame kind = %d, want video", video.Kind)
	}
	if video.Flags&protocol.FlagKeyframe == 0 {
		t.Fatal("published IDR was not flagged as a keyframe")
	}
	if string(video.Payload) != string(keyframe) {
		t.Fatalf("video payload = %x, want %x", video.Payload, keyframe)
	}

	opus := []byte{0xF8, 0xFF, 0xFE}
	if err := publisher.WriteAudio(&rtp.Packet{Timestamp: 960, Payload: opus}, time.Now()); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	audio := readMedia(t, viewer)
	if audio.Kind != protocol.MediaAudio {
		t.Fatalf("frame kind = %d, want audio", audio.Kind)
	}
	if string(audio.Payload) != string(opus) {
		t.Fatalf("audio payload = %x, want %x", audio.Payload, opus)
	}
}

// TestWebSocketOutputKeepsRetryingWithoutRelay pins the promise that a stream
// still starts when the Activity stack is not up yet: construction succeeds and
// later frames are dropped rather than blocking the capture pipeline.
func TestWebSocketOutputKeepsRetryingWithoutRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	publisher, err := output.NewWebSocketOutput(ctx, output.WebSocketOptions{
		Endpoint: "ws://" + freeLoopbackAddr(t),
		Room:     "room-2",
		Token:    "publish-token",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	defer publisher.Close()

	keyframe := []byte{0, 0, 0, 1, 0x67, 0x42, 0xE0, 0x1F, 0, 0, 0, 1, 0x65, 0x88}
	done := make(chan error, 1)
	go func() { done <- publisher.WriteVideo(keyframe, time.Now(), 0) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write video: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WriteVideo blocked while the relay was unreachable")
	}
}

func TestViewerReceivesMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relayAddr := freeLoopbackAddr(t)
	relaySrv := relay.NewServer(testLogger(), "publish-token", 5)
	go func() { _ = relaySrv.Run(ctx, relayAddr) }()

	deadline := time.Now().Add(5 * time.Second)
	for relaySrv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	pub, err := output.NewWebSocketOutput(ctx, output.WebSocketOptions{
		Endpoint: "ws://" + relayAddr,
		Room:     "room-test",
		Token:    "publish-token",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	time.Sleep(100 * time.Millisecond)

	t0 := time.Now()
	keyframe := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xC0, 0x2A,
		0, 0, 0, 1, 0x68, 0xCE, 0x3C, 0x80,
		0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00,
	}
	_ = pub.WriteVideo(keyframe, t0, time.Second/60)

	for i := 1; i <= 5; i++ {
		delta := []byte{0, 0, 0, 1, 0x41, 0x9A, 0x01}
		_ = pub.WriteVideo(delta, t0.Add(time.Duration(i*16)*time.Millisecond), time.Second/60)
	}

	time.Sleep(200 * time.Millisecond)

	viewer, _, err := websocket.DefaultDialer.Dial("ws://"+relayAddr+"/v1/watch/room-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	_ = viewer.WriteJSON(map[string]any{"v": 1, "type": "hello", "role": "viewer"})

	var msg map[string]any
	_ = viewer.ReadJSON(&msg)
	if msg["type"] != "ready" {
		t.Fatalf("handshake = %v, want ready", msg)
	}

	// Publisher sends delta 6 - viewer must NOT receive it because it missed references
	tNow := time.Now()
	deltaNow := []byte{0, 0, 0, 1, 0x41, 0x9A, 0x02}
	_ = pub.WriteVideo(deltaNow, tNow, time.Second/60)

	// Publisher sends live keyframe 2 - viewer MUST receive this first
	keyframe2 := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xC0, 0x2A,
		0, 0, 0, 1, 0x68, 0xCE, 0x3C, 0x80,
		0, 0, 0, 1, 0x65, 0x88, 0x84, 0x01,
	}
	_ = pub.WriteVideo(keyframe2, tNow.Add(16*time.Millisecond), time.Second/60)

	f1 := readMedia(t, viewer)
	if f1.Flags&protocol.FlagKeyframe == 0 {
		t.Fatalf("first frame received was not a keyframe: flags=%d", f1.Flags)
	}
	t.Logf("Frame 1 (live keyframe): Kind=%d Flags=%d TS=%d", f1.Kind, f1.Flags, f1.Timestamp)

	// Publisher sends delta 7 - viewer MUST receive it now that it has a keyframe reference
	delta7 := []byte{0, 0, 0, 1, 0x41, 0x9A, 0x03}
	_ = pub.WriteVideo(delta7, tNow.Add(32*time.Millisecond), time.Second/60)

	f2 := readMedia(t, viewer)
	if f2.Flags&protocol.FlagKeyframe != 0 {
		t.Fatalf("second frame was expected to be a delta: flags=%d", f2.Flags)
	}
	t.Logf("Frame 2 (delta): Kind=%d Flags=%d TS=%d", f2.Kind, f2.Flags, f2.Timestamp)
}


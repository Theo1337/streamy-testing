package output

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestWebSocketOutputResynchronisesAfterOverflow pins the back-pressure policy:
// a full queue is not allowed to grow, and the frames that follow the gap must
// not be sent until an IDR makes the stream decodable again.
func TestWebSocketOutputResynchronisesAfterOverflow(t *testing.T) {
	out := &WebSocketOutput{
		opts:  WebSocketOptions{Room: "test"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		queue: make(chan outbound, 2),
		done:  make(chan struct{}),
	}

	delta := []byte{0, 0, 0, 1, 0x41, 0x9a}
	keyframe := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f,
		0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80,
		0, 0, 0, 1, 0x65, 0x80,
	}
	now := time.Now()

	if err := out.WriteVideo(delta, now, 0); err != nil {
		t.Fatalf("first delta: %v", err)
	}
	if err := out.WriteVideo(delta, now, 0); err != nil {
		t.Fatalf("second delta: %v", err)
	}
	// This one overflows the queue. It is a delta, so it must be discarded
	// rather than queued after the frames that were just dropped.
	if err := out.WriteVideo(delta, now, 0); err != nil {
		t.Fatalf("overflowing delta: %v", err)
	}
	if pending := len(out.queue); pending != 0 {
		t.Fatalf("queue holds %d frames after overflowing, want 0", pending)
	}
	if !out.needsKeyframe.Load() {
		t.Fatal("output was not marked for resynchronisation")
	}

	// Deltas are ignored until the encoder produces an IDR.
	if err := out.WriteVideo(delta, now, 0); err != nil {
		t.Fatalf("delta after the gap: %v", err)
	}
	if pending := len(out.queue); pending != 0 {
		t.Fatalf("a delta was queued after the gap: %d pending", pending)
	}

	if err := out.WriteVideo(keyframe, now, 0); err != nil {
		t.Fatalf("keyframe: %v", err)
	}
	if pending := len(out.queue); pending != 1 {
		t.Fatalf("keyframe was not queued: %d pending", pending)
	}
	queued := <-out.queue
	if queued.flags&relayKeyframe == 0 {
		t.Fatalf("queued frame flags = %#[1]x, want the keyframe flag", queued.flags)
	}

	// And the stream continues with deltas again.
	if err := out.WriteVideo(delta, now, 0); err != nil {
		t.Fatalf("delta after the keyframe: %v", err)
	}
	if pending := len(out.queue); pending != 1 {
		t.Fatalf("the stream did not resume after the keyframe: %d pending", pending)
	}
}

// TestWebSocketOutputKeepsAudioAfterOverflow makes sure the resynchronisation
// applies to video only: a viewer should still hear audio while it waits for
// the next keyframe.
func TestWebSocketOutputKeepsAudioAfterOverflow(t *testing.T) {
	out := &WebSocketOutput{
		opts:  WebSocketOptions{Room: "test"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		queue: make(chan outbound, 1),
		done:  make(chan struct{}),
	}
	out.enqueue(outbound{kind: relayVideo, timestamp: 1})
	out.enqueue(outbound{kind: relayAudio, timestamp: 2})

	if pending := len(out.queue); pending != 1 {
		t.Fatalf("queue holds %d messages, want the audio packet", pending)
	}
	if !out.needsKeyframe.Load() {
		t.Fatal("dropping video did not request a keyframe")
	}
	if msg := <-out.queue; msg.kind != relayAudio {
		t.Fatalf("queued message kind = %d, want audio", msg.kind)
	}
}

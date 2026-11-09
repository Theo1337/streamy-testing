package output

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
)

const (
	relayProtocolVersion = 1
	relayHeaderSize      = 10
	relayVideo           = 0x01
	relayAudio           = 0x02
	relayKeyframe        = 0x01
)

// relayQueueDepth is how many media messages may wait for the socket.
//
// The queue exists to smooth a burst, not to absorb seconds of video: a live
// stream that queues four seconds of frames is four seconds behind, and no
// viewer can undo that. At 60 frames plus 50 audio packets a second this is
// roughly half a second of slack.
const relayQueueDepth = 64

// WebSocketOptions configures the relay publisher.
type WebSocketOptions struct {
	// Endpoint is the relay base URL, e.g. wss://relay.example.com.
	Endpoint string
	// Room is the Discord channel ID used as the relay room.
	Room string
	// Token is the deployment-wide publisher secret.
	Token  string
	Logger *slog.Logger
}

// WebSocketOutput publishes the same H.264/Opus stream to the Go relay. It is
// deliberately lossy under back-pressure: a live stream must not block capture
// because the relay or a viewer is slow.
type WebSocketOutput struct {
	opts WebSocketOptions
	log  *slog.Logger

	queue chan outbound
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
	conn  *websocket.Conn

	// needsKeyframe is set when frames were dropped: a decoder cannot resume
	// from a delta that references a picture it never saw, so publishing waits
	// for the next IDR instead of corrupting the picture for a whole GOP.
	needsKeyframe atomic.Bool
	// dropping makes the "queue is full" warning fire once per episode rather
	// than once per audio packet.
	dropping atomic.Bool

	sendMu sync.Mutex
	// bytesSinceLog is reset by the writer goroutine.
	bytesSinceLog int64
	lastLog       time.Time

	audioMu       sync.Mutex
	audioClockSet bool
	audioBaseRTP  uint32
	audioBaseTime time.Time

	h264Mu sync.Mutex
	sps    []byte
	pps    []byte
}

type outbound struct {
	kind      byte
	flags     byte
	timestamp uint64
	payload   []byte
}

// NewWebSocketOutput prepares the relay publisher.
//
// A connection is attempted once, with a short deadline, so a bad URL or token
// is visible at stream start. Failure is not fatal: the local Activity stack
// can still be booting (it builds a gateway and waits for a tunnel), and
// blocking the start of a stream on it would make the hotkey feel broken.
// run keeps retrying in the background and publishes as soon as the relay
// answers.
func NewWebSocketOutput(ctx context.Context, opts WebSocketOptions) (*WebSocketOutput, error) {
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, errors.New("relay URL is empty")
	}
	if strings.TrimSpace(opts.Room) == "" {
		return nil, errors.New("relay room is empty")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	o := &WebSocketOutput{
		opts:  opts,
		log:   opts.Logger,
		queue: make(chan outbound, relayQueueDepth),
		done:  make(chan struct{}),
	}

	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	conn, err := o.dial(dialCtx)
	cancel()
	if err != nil {
		o.log.Warn("relay is not reachable yet; the stream will connect in the background",
			"room", opts.Room, "error", err)
		o.needsKeyframe.Store(true)
	} else {
		o.conn = conn
	}
	o.lastLog = time.Now()
	o.wg.Add(1)
	go o.run(ctx)
	return o, nil
}

// Name identifies the output in logs.
func (o *WebSocketOutput) Name() string { return "websocket" }

// WriteVideo sends one Annex-B access unit with a keyframe flag.
func (o *WebSocketOutput) WriteVideo(sample []byte, timestamp time.Time, _ time.Duration) error {
	if len(sample) == 0 {
		return errors.New("cannot publish an empty H.264 access unit")
	}
	info := inspectH264(sample)
	keyframe := info.hasIDR
	if o.needsKeyframe.Load() && !keyframe {
		// The viewer missed pictures, so this delta cannot be decoded. The next
		// IDR is at most a keyframe interval away.
		return nil
	}
	if keyframe {
		o.needsKeyframe.Store(false)
	}

	o.h264Mu.Lock()
	if info.sps != nil {
		o.sps = append(o.sps[:0], info.sps...)
	}
	if info.pps != nil {
		o.pps = append(o.pps[:0], info.pps...)
	}
	sample = ensureParameterSets(sample, info, o.sps, o.pps)
	o.h264Mu.Unlock()

	flags := byte(0)
	if keyframe {
		flags |= relayKeyframe
	}
	o.enqueue(outbound{
		kind:      relayVideo,
		flags:     flags,
		timestamp: uint64(timestamp.UnixMilli()),
		payload:   append([]byte(nil), sample...),
	})
	return nil
}

// WriteAudio sends the Opus payload from the RTP packet. The RTP timestamp is
// mapped onto the same wall clock as video, so the browser can schedule both
// streams together.
func (o *WebSocketOutput) WriteAudio(packet *rtp.Packet, received time.Time) error {
	if packet == nil || len(packet.Payload) == 0 {
		return errors.New("cannot publish an empty Opus packet")
	}
	o.audioMu.Lock()
	if !o.audioClockSet {
		o.audioClockSet = true
		o.audioBaseRTP = packet.Timestamp
		o.audioBaseTime = received
	}
	delta := packet.Timestamp - o.audioBaseRTP
	// A backwards jump means the encoder restarted its timestamp base.
	if delta > 1<<31 {
		o.audioBaseRTP = packet.Timestamp
		o.audioBaseTime = received
		delta = 0
	}
	timestamp := o.audioBaseTime.Add(time.Duration(delta) * time.Second / 48000)
	o.audioMu.Unlock()

	o.enqueue(outbound{
		kind:      relayAudio,
		timestamp: uint64(timestamp.UnixMilli()),
		payload:   append([]byte(nil), packet.Payload...),
	})
	return nil
}

// Close flushes nothing (live media has no useful tail) and releases the socket.
func (o *WebSocketOutput) Close() error {
	o.once.Do(func() {
		close(o.done)
		o.sendMu.Lock()
		if o.conn != nil {
			_ = o.conn.Close()
			o.conn = nil
		}
		o.sendMu.Unlock()
	})
	o.wg.Wait()
	return nil
}

func (o *WebSocketOutput) enqueue(frame outbound) {
	select {
	case o.queue <- frame:
		if o.dropping.Swap(false) {
			o.log.Info("relay output caught up", "room", o.opts.Room)
		}
		return
	default:
	}

	// The socket cannot keep up. Keeping the backlog would only add latency,
	// and sending deltas across a gap would corrupt the picture until the next
	// keyframe anyway, so drop everything queued and resume at the next IDR.
	// A keyframe that is being queued right now is the perfect place to resume.
	dropped := 0
drain:
	for {
		select {
		case <-o.queue:
			dropped++
		default:
			break drain
		}
	}

	video := frame.kind == relayVideo
	keyframe := video && frame.flags&relayKeyframe != 0
	if !keyframe {
		o.needsKeyframe.Store(true)
	}
	if !o.dropping.Swap(true) {
		o.log.Warn("relay output fell behind; dropped the backlog and will resync on a keyframe",
			"dropped", dropped, "kind", frame.kind)
	}
	if video && !keyframe {
		// Dropping a delta is what keeps the decoder from showing garbage
		// until the next IDR.
		return
	}
	select {
	case o.queue <- frame:
	default:
	}
}

func (o *WebSocketOutput) currentConn() *websocket.Conn {
	o.sendMu.Lock()
	defer o.sendMu.Unlock()
	return o.conn
}

func (o *WebSocketOutput) setConn(conn *websocket.Conn) {
	o.sendMu.Lock()
	o.conn = conn
	o.sendMu.Unlock()
}

// closeConn closes and forgets the current socket, if any.
func (o *WebSocketOutput) closeConn() {
	o.sendMu.Lock()
	conn := o.conn
	o.conn = nil
	o.sendMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (o *WebSocketOutput) run(ctx context.Context) {
	defer o.wg.Done()
	backoff := time.Second

	for {
		if o.currentConn() == nil {
			dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			conn, err := o.dial(dialCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				o.log.Warn("relay connect failed; retrying", "error", err, "retry_in", backoff.Round(time.Millisecond))
				if !o.sleep(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			o.setConn(conn)
			o.needsKeyframe.Store(true)
			backoff = time.Second
		}

		err := o.writeLoop(ctx)
		o.closeConn()
		if ctx.Err() != nil || !o.isOpen() {
			return
		}
		if err != nil {
			o.log.Warn("relay connection lost; reconnecting", "error", err, "retry_in", backoff.Round(time.Millisecond))
		}
		if !o.sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// sleep waits for d, returning false when the output is shutting down.
func (o *WebSocketOutput) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-o.done:
		return false
	case <-timer.C:
		return true
	}
}

func (o *WebSocketOutput) isOpen() bool {
	select {
	case <-o.done:
		return false
	default:
		return true
	}
}

func (o *WebSocketOutput) writeLoop(ctx context.Context) error {
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-o.done:
			return nil
		case frame := <-o.queue:
			o.sendMu.Lock()
			conn := o.conn
			if conn != nil {
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			}
			o.sendMu.Unlock()
			if conn == nil {
				continue
			}
			data := encodeRelayFrame(frame)
			if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				return err
			}
			o.recordBytes(int64(len(data)))
		case <-ping.C:
			o.sendMu.Lock()
			conn := o.conn
			if conn != nil {
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			}
			o.sendMu.Unlock()
			if conn == nil {
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage,
				[]byte(fmt.Sprintf(`{"v":%d,"type":"ping","t":%d}`, relayProtocolVersion, time.Now().UnixMilli()))); err != nil {
				return err
			}
		}
	}
}

func (o *WebSocketOutput) recordBytes(count int64) {
	o.bytesSinceLog += count
	elapsed := time.Since(o.lastLog)
	if elapsed < 10*time.Second {
		return
	}
	o.log.Info("relay publisher bandwidth",
		"room", o.opts.Room,
		"estimated_kbps", float64(o.bytesSinceLog)*8/elapsed.Seconds()/1000,
	)
	o.bytesSinceLog = 0
	o.lastLog = time.Now()
}

func (o *WebSocketOutput) dial(ctx context.Context) (*websocket.Conn, error) {
	endpoint, err := relayPublishURL(o.opts.Endpoint, o.opts.Room)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, response, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("connect to relay (HTTP %s): %w", response.Status, err)
		}
		return nil, fmt.Errorf("connect to relay: %w", err)
	}
	hello := map[string]any{
		"v": relayProtocolVersion, "type": "hello", "role": "publisher", "token": o.opts.Token,
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(hello); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send relay hello: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ready struct {
		Version int    `json:"v"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := conn.ReadJSON(&ready); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read relay welcome: %w", err)
	}
	if ready.Type == "error" {
		_ = conn.Close()
		return nil, fmt.Errorf("relay rejected publisher (%s): %s", ready.Code, ready.Message)
	}
	if ready.Type != "ready" || ready.Version != relayProtocolVersion {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected relay handshake response %q", ready.Type)
	}
	_ = conn.SetReadDeadline(time.Time{})
	o.log.Info("connected to relay", "room", o.opts.Room)
	return conn, nil
}

func relayPublishURL(endpoint, room string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("parse relay URL: %w", err)
	}
	if (base.Scheme != "wss" && base.Scheme != "ws") || base.Host == "" {
		return "", errors.New("relay URL must be an absolute ws or wss URL")
	}
	base.Path = path.Join(base.Path, "v1", "publish", room)
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

func encodeRelayFrame(frame outbound) []byte {
	data := make([]byte, relayHeaderSize+len(frame.payload))
	data[0] = frame.kind
	data[1] = frame.flags
	binary.BigEndian.PutUint64(data[2:10], frame.timestamp)
	copy(data[10:], frame.payload)
	return data
}

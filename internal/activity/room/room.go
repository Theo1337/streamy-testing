package room

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"streamy/internal/activity/protocol"
)

const (
	ViewerQueueDepth = 64
	PingInterval     = 25 * time.Second
	ReadDeadline     = 70 * time.Second
)

type Outbound struct {
	MessageType int
	Data        []byte
}

type ControlMessage struct {
	Version int    `json:"v"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Token   string `json:"token"`
	Time    int64  `json:"t"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RelayClient struct {
	Conn      *websocket.Conn
	Role      string
	Send      chan Outbound
	Done      chan struct{}
	closeOnce sync.Once
	Room      *Room

	needsKeyframe atomic.Bool
}

func (c *RelayClient) SetNeedsKeyframe(needs bool) {
	c.needsKeyframe.Store(needs)
}

func (c *RelayClient) trySend(message Outbound) bool {
	select {
	case c.Send <- message:
		return true
	default:
		return false
	}
}

func (c *RelayClient) TrySendMedia(message Outbound, video, keyframe bool) bool {
	if video {
		if c.needsKeyframe.Load() && !keyframe {
			return false
		}
		if keyframe {
			c.needsKeyframe.Store(false)
		}
	}
	select {
	case c.Send <- message:
		return true
	default:
	}

	// Drop backlog and wait for keyframe
	for {
		select {
		case <-c.Send:
		default:
			if !keyframe {
				c.needsKeyframe.Store(true)
			}
			if video && !keyframe {
				return false
			}
			select {
			case c.Send <- message:
				return true
			default:
				return false
			}
		}
	}
}

func (c *RelayClient) SendJSON(v any) bool {
	data, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return c.trySend(Outbound{MessageType: websocket.TextMessage, Data: data})
}

func (c *RelayClient) Close(code int, reason string) {
	c.closeOnce.Do(func() {
		close(c.Done)
		c.Conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason),
			time.Now().Add(5*time.Second))
		c.Conn.Close()
	})
}

func (c *RelayClient) Writer() {
	ping := time.NewTicker(PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-c.Done:
			return
		case message := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(message.MessageType, message.Data); err != nil {
				return
			}
		case <-ping.C:
			if err := c.Conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}

type Room struct {
	id string

	mu        sync.Mutex
	publisher *RelayClient
	viewers   map[*RelayClient]struct{}

	sps            []byte
	pps            []byte
	keyframe       []byte
	keyframeStamp  uint64
	lastVideoStamp uint64

	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64
}

func NewRoom(id string) *Room {
	return &Room{
		id:      id,
		viewers: make(map[*RelayClient]struct{}),
	}
}

func (r *Room) ID() string {
	return r.id
}

func (r *Room) Publisher() *RelayClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.publisher
}

func (r *Room) AddPublisher(c *RelayClient) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.publisher != nil {
		return false
	}
	r.publisher = c
	return true
}

func (r *Room) RemovePublisher(c *RelayClient) {
	r.mu.Lock()
	if r.publisher == c {
		r.publisher = nil
	}
	r.mu.Unlock()
}

func (r *Room) AddViewer(c *RelayClient, maxViewers int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if maxViewers > 0 && len(r.viewers) >= maxViewers {
		return false
	}
	r.viewers[c] = struct{}{}
	return true
}

func (r *Room) RemoveViewer(c *RelayClient) {
	r.mu.Lock()
	delete(r.viewers, c)
	r.mu.Unlock()
}

func (r *Room) ViewerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.viewers)
}

// Empty reports whether the room holds no publisher and no viewers. It takes
// the lock itself: dropRoom must not call Publisher or ViewerCount while
// already holding r.mu, or the room deadlocks and every later viewer for the
// same room hangs before it can be accepted.
func (r *Room) Empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.publisher == nil && len(r.viewers) == 0
}

func (r *Room) Publish(frame protocol.MediaFrame, encoded []byte) {
	r.bytesIn.Add(uint64(len(encoded)))

	r.mu.Lock()
	keyframe := false
	if frame.Kind == protocol.MediaVideo {
		r.lastVideoStamp = frame.Timestamp
		info := inspectH264(frame.Payload)
		if info.sps != nil {
			r.sps = info.sps
		}
		if info.pps != nil {
			r.pps = info.pps
		}
		keyframe = info.hasIDR
		if keyframe {
			r.keyframe = append(r.keyframe[:0], frame.Payload...)
			r.keyframeStamp = frame.Timestamp
		}
	}
	viewers := make([]*RelayClient, 0, len(r.viewers))
	for viewer := range r.viewers {
		viewers = append(viewers, viewer)
	}
	r.mu.Unlock()

	for _, viewer := range viewers {
		if viewer.TrySendMedia(Outbound{MessageType: websocket.BinaryMessage, Data: encoded}, frame.Kind == protocol.MediaVideo, keyframe) {
			r.bytesOut.Add(uint64(len(encoded)))
		}
	}
}

func (r *Room) SendCachedKeyframe(c *RelayClient, now uint64) {
	r.mu.Lock()
	keyframe := append([]byte(nil), r.keyframe...)
	sps := append([]byte(nil), r.sps...)
	pps := append([]byte(nil), r.pps...)
	if r.lastVideoStamp > 0 {
		now = r.lastVideoStamp
	}
	r.mu.Unlock()
	if len(keyframe) == 0 {
		return
	}
	keyframe = addMissingParameterSets(keyframe, sps, pps)
	encoded := protocol.EncodeMediaFrame(protocol.MediaVideo, protocol.FlagKeyframe, now, keyframe)
	if c.TrySendMedia(Outbound{MessageType: websocket.BinaryMessage, Data: encoded}, true, true) {
		r.bytesOut.Add(uint64(len(encoded)))
	}
}

func (r *Room) CloseViewers(code int, reason string) {
	r.mu.Lock()
	viewers := make([]*RelayClient, 0, len(r.viewers))
	for viewer := range r.viewers {
		viewers = append(viewers, viewer)
	}
	r.viewers = make(map[*RelayClient]struct{})
	r.mu.Unlock()
	for _, viewer := range viewers {
		viewer.SendJSON(map[string]any{
			"v": protocol.ProtocolVersion, "type": "error", "code": "publisher-gone", "message": reason,
		})
		viewer.Close(code, reason)
	}
}

func (r *Room) Stats() (viewers int, bytesIn, bytesOut uint64) {
	return r.ViewerCount(), r.bytesIn.Swap(0), r.bytesOut.Swap(0)
}

type h264Info struct {
	sps    []byte
	pps    []byte
	hasIDR bool
}

// inspectH264 reads one Annex-B access unit. The publisher hands the relay
// ffmpeg's Annex-B output (start codes, not 4-byte length prefixes), so the NAL
// header is the byte after the start code. Parsing it as a length prefix made
// every frame look like a giant unknown NAL: no keyframe was ever cached and a
// viewer joining mid-stream got deltas it could not decode.
func inspectH264(payload []byte) h264Info {
	var info h264Info
	for _, nal := range splitAnnexB(payload) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1F {
		case 7: // SPS
			info.sps = append([]byte{0, 0, 0, 1}, nal...)
		case 8: // PPS
			info.pps = append([]byte{0, 0, 0, 1}, nal...)
		case 5: // IDR
			info.hasIDR = true
		}
	}
	return info
}

// splitAnnexB returns the NAL units of an Annex-B buffer without their start
// codes.
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	for i := 0; i+2 < len(data); {
		startCode := 0
		switch {
		case data[i] == 0 && data[i+1] == 0 && data[i+2] == 1:
			startCode = 3
		case i+3 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1:
			startCode = 4
		}
		if startCode == 0 {
			i++
			continue
		}
		if start >= 0 {
			nals = append(nals, data[start:i])
		}
		start = i + startCode
		i = start
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, data[start:])
	}
	return nals
}

// addMissingParameterSets makes a cached keyframe self-contained without
// repeating parameter sets that it already carries.
func addMissingParameterSets(payload []byte, sps, pps []byte) []byte {
	info := inspectH264(payload)
	var prefix []byte
	if info.sps == nil && len(sps) > 0 {
		prefix = append(prefix, sps...)
	}
	if info.pps == nil && len(pps) > 0 {
		prefix = append(prefix, pps...)
	}
	if len(prefix) == 0 {
		return payload
	}
	result := make([]byte, 0, len(prefix)+len(payload))
	result = append(result, prefix...)
	result = append(result, payload...)
	return result
}

package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"

	"streamy/internal/activity/bot"
	"streamy/internal/activity/gateway"
	"streamy/internal/activity/protocol"
	"streamy/internal/activity/relay"
	"streamy/internal/output"
)

// discardLogger keeps the servers quiet during tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freeAddr reserves an ephemeral loopback port and releases it. The window
// between closing the listener and the server binding it is tiny, and the
// alternative is threading listeners through every service.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}

func waitHealthy(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	client := http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not become healthy", baseURL)
}

type testStack struct {
	relayAddr   string
	gatewayAddr string
	botAddr     string
}

// startStack brings up the relay, the bot and the gateway exactly as
// `streamy activity` does, and waits for the gateway to answer.
func startStack(t *testing.T) testStack {
	t.Helper()
	logger := discardLogger()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	relayAddr := freeAddr(t)
	relaySrv := relay.NewServer(logger, "publish-token", 10)
	go func() { _ = relaySrv.Run(ctx, relayAddr) }()

	botAddr := freeAddr(t)
	gatewayAddr := freeAddr(t)
	botSrv, err := bot.NewBot(logger, bot.Config{
		DiscordClientID:     "test-client",
		DiscordClientSecret: "test-secret",
		IdentitySecret:      "0123456789abcdef0123456789abcdef",
		RelayAddr:           relayAddr,
		GatewayAddr:         gatewayAddr,
		BotAddr:             botAddr,
		MaxViewers:          10,
	})
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}
	go func() { _ = botSrv.Run(ctx, botAddr) }()

	// The gateway resolves the relay and bot URLs from Addr(), which is set by
	// Run before it starts serving. Wait for both to be published.
	waitForAddr(t, relaySrv.Addr, "relay")
	waitForAddr(t, botSrv.Addr, "bot")

	gatewaySrv := gateway.NewGateway(logger, botSrv, relaySrv, "")
	go func() { _ = gatewaySrv.Run(ctx, gatewayAddr) }()
	waitHealthy(t, "http://"+gatewayAddr)

	return testStack{
		relayAddr:   relayAddr,
		gatewayAddr: gatewayAddr,
		botAddr:     botAddr,
	}
}

func waitForAddr(t *testing.T, addr func() string, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr() != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not publish an address", name)
}

// dialRelay opens a relay WebSocket through the gateway's /relay proxy,
// performing the role handshake the desktop app and the viewer use.
func dialRelay(t *testing.T, gatewayAddr, role, room, token string) (*websocket.Conn, func()) {
	t.Helper()
	segment := "watch"
	if role == "publisher" {
		segment = "publish"
	}
	return dialRelayPath(t, gatewayAddr, "/.proxy/relay/v1/"+segment+"/"+room, role, token)
}

// dialRelayPath is dialRelay for an explicit public path, so the tests can
// cover every prefix shape Discord and the Worker can produce.
func dialRelayPath(t *testing.T, gatewayAddr, path, role, token string) (*websocket.Conn, func()) {
	t.Helper()
	url := "ws://" + gatewayAddr + path
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", role, err)
	}

	hello := map[string]any{"v": protocol.ProtocolVersion, "type": "hello", "role": role, "token": token}
	if err := conn.WriteJSON(hello); err != nil {
		conn.Close()
		t.Fatalf("%s hello: %v", role, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ready struct {
		Type  string `json:"type"`
		Code  string `json:"code"`
		Role  string `json:"role"`
		Error string `json:"message"`
	}
	if err := conn.ReadJSON(&ready); err != nil {
		conn.Close()
		t.Fatalf("%s ready: %v", role, err)
	}
	if ready.Type != "ready" {
		conn.Close()
		t.Fatalf("%s was not accepted: %+v", role, ready)
	}
	if ready.Role != role {
		conn.Close()
		t.Fatalf("%s ready reported role %q", role, ready.Role)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, func() { conn.Close() }
}

// readMedia returns the next binary media frame, failing on text messages or
// when the deadline passes.
func readMedia(t *testing.T, conn *websocket.Conn, within time.Duration) protocol.MediaFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("expected a binary media frame, got message type %d: %s", messageType, data)
	}
	frame, err := protocol.ParseMediaFrame(data)
	if err != nil {
		t.Fatalf("parse media frame: %v", err)
	}
	return frame
}

// TestStreamFlowsThroughGateway is the end-to-end check for the Activity path:
// a publisher connects through the gateway proxy, a viewer connects through the
// same proxy, and the encoded H.264 and Opus frames reach the viewer.
func TestStreamFlowsThroughGateway(t *testing.T) {
	stack := startStack(t)

	publisher, closePublisher := dialRelay(t, stack.gatewayAddr, "publisher", "room-1", "publish-token")
	defer closePublisher()
	viewer, closeViewer := dialRelay(t, stack.gatewayAddr, "viewer", "room-1", "")
	defer closeViewer()

	// A real access unit is Annex-B: SPS, PPS and an IDR slice. The relay has
	// to recognise it as a keyframe so a viewer that joins later gets it.
	sps := []byte{0, 0, 0, 1, 0x67, 0x42, 0xE0, 0x1F}
	pps := []byte{0, 0, 0, 1, 0x68, 0xCE, 0x3C, 0x80}
	idr := []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00}
	keyframe := append(append(append([]byte(nil), sps...), pps...), idr...)

	if err := publisher.WriteMessage(websocket.BinaryMessage,
		protocol.EncodeMediaFrame(protocol.MediaVideo, protocol.FlagKeyframe, 1000, keyframe)); err != nil {
		t.Fatalf("publish keyframe: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// A viewer joining after the keyframe must receive the cached keyframe
	// before any delta frame, or it cannot decode the stream.
	cached := readMedia(t, viewer, 2*time.Second)
	if cached.Kind != protocol.MediaVideo {
		t.Fatalf("cached frame kind = %d, want video", cached.Kind)
	}
	if cached.Flags&protocol.FlagKeyframe == 0 {
		t.Fatalf("cached frame was not marked as a keyframe")
	}

	delta := []byte{0, 0, 0, 1, 0x41, 0x9A, 0x22}
	if err := publisher.WriteMessage(websocket.BinaryMessage,
		protocol.EncodeMediaFrame(protocol.MediaVideo, 0, 1033, delta)); err != nil {
		t.Fatalf("publish delta: %v", err)
	}
	live := readMedia(t, viewer, 2*time.Second)
	if live.Kind != protocol.MediaVideo || live.Timestamp != 1033 {
		t.Fatalf("live frame = kind %d timestamp %d, want video at 1033", live.Kind, live.Timestamp)
	}
	if string(live.Payload) != string(delta) {
		t.Fatalf("live payload = %x, want %x", live.Payload, delta)
	}

	audio := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	if err := publisher.WriteMessage(websocket.BinaryMessage,
		protocol.EncodeMediaFrame(protocol.MediaAudio, 0, 1040, audio)); err != nil {
		t.Fatalf("publish audio: %v", err)
	}
	gotAudio := readMedia(t, viewer, 2*time.Second)
	if gotAudio.Kind != protocol.MediaAudio {
		t.Fatalf("audio frame kind = %d, want audio", gotAudio.Kind)
	}
	if string(gotAudio.Payload) != string(audio) {
		t.Fatalf("audio payload = %x, want %x", gotAudio.Payload, audio)
	}
}

// TestPublisherTokenIsRequired makes sure the relay does not accept a
// publisher without the deployment token when one is configured.
func TestPublisherTokenIsRequired(t *testing.T) {
	stack := startStack(t)

	conn, _, err := websocket.DefaultDialer.Dial(
		"ws://"+stack.gatewayAddr+"/.proxy/relay/v1/publish/room-2", nil)
	if err != nil {
		t.Fatalf("dial publisher: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{
		"v": protocol.ProtocolVersion, "type": "hello", "role": "publisher", "token": "wrong",
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var reply struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	if err := conn.ReadJSON(&reply); err != nil {
		t.Fatalf("read rejection: %v", err)
	}
	if reply.Type != "error" || reply.Code != "unauthorized" {
		t.Fatalf("reply = %+v, want an unauthorized error", reply)
	}
}

// TestViewerPageIsServed makes sure the gateway serves the real WebCodecs
// viewer at "/", not a placeholder and not the bot API.
func TestViewerPageIsServed(t *testing.T) {
	stack := startStack(t)

	response, err := http.Get("http://" + stack.gatewayAddr + "/")
	if err != nil {
		t.Fatalf("get viewer: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("viewer status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read viewer: %v", err)
	}
	page := string(body)
	for _, want := range []string{"VideoDecoder", "/.proxy/relay"} {
		if !strings.Contains(page, want) {
			t.Fatalf("viewer page does not mention %q", want)
		}
	}
}

// TestDesktopPublisherToViewerThroughGateway mirrors the deployed layout: the
// desktop app publishes through ws://gateway/.proxy/relay and the viewer watches
// through the same host.
func TestDesktopPublisherToViewerThroughGateway(t *testing.T) {
	stack := startStack(t)

	viewer, closeViewer := dialRelay(t, stack.gatewayAddr, "viewer", "room-desktop", "")
	defer closeViewer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publisher, err := output.NewWebSocketOutput(ctx, output.WebSocketOptions{
		Endpoint: "ws://" + stack.gatewayAddr + "/.proxy/relay",
		Room:     "room-desktop",
		Token:    "publish-token",
		Logger:   discardLogger(),
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
	video := readMedia(t, viewer, 2*time.Second)
	if video.Kind != protocol.MediaVideo || video.Flags&protocol.FlagKeyframe == 0 {
		t.Fatalf("video frame = kind %d flags %d, want a keyframe", video.Kind, video.Flags)
	}
	if string(video.Payload) != string(keyframe) {
		t.Fatalf("video payload = %x, want %x", video.Payload, keyframe)
	}

	opus := []byte{0xF8, 0xFF, 0xFE}
	if err := publisher.WriteAudio(&rtp.Packet{Timestamp: 960, Payload: opus}, time.Now()); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	audio := readMedia(t, viewer, 2*time.Second)
	if audio.Kind != protocol.MediaAudio || string(audio.Payload) != string(opus) {
		t.Fatalf("audio frame = kind %d payload %x, want %x", audio.Kind, audio.Payload, opus)
	}
}

// TestRelayPathShapes covers the prefixes the viewer can arrive with. Discord
// strips /.proxy and the URL-mapping prefix in stages, and the Worker forwards
// whatever path is left, so the gateway has to accept all of them.
func TestRelayPathShapes(t *testing.T) {
	stack := startStack(t)

	paths := []string{
		"/.proxy/relay/v1/watch/room-shapes",
		"/relay/v1/watch/room-shapes",
		"/v1/watch/room-shapes",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			viewer, closeViewer := dialRelayPath(t, stack.gatewayAddr, path, "viewer", "")
			defer closeViewer()
			if viewer == nil {
				t.Fatalf("no connection for %s", path)
			}
		})
	}
}

// TestViewerCanRejoinRoom is a regression test for a room-lock deadlock: when
// the last viewer of a room left, dropRoom locked the room and then called
// Publisher/ViewerCount, which lock it again. The room never unlocked, so every
// later viewer for that channel hung before it was accepted — the Activity sat
// on "Reconnecting" until the stack restarted.
func TestViewerCanRejoinRoom(t *testing.T) {
	stack := startStack(t)

	for attempt := 1; attempt <= 3; attempt++ {
		viewer, closeViewer := dialRelay(t, stack.gatewayAddr, "viewer", "rejoin-room", "")
		if viewer == nil {
			t.Fatalf("attempt %d: no viewer", attempt)
		}
		closeViewer()
	}
}

// TestTokenExchangeIsRouted checks the exact /token path reaches the bot. An
// empty body must come back as the bot's 400 rather than the viewer's 404: a
// 404 is what left the Activity stuck before the route existed.
func TestTokenExchangeIsRouted(t *testing.T) {
	stack := startStack(t)
	base := "http://" + stack.gatewayAddr

	for _, path := range []string{"/token", "/api/token", "/.proxy/api/token"} {
		t.Run(path, func(t *testing.T) {
			response, err := http.Post(base+path, "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatalf("post %s: %v", path, err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("%s status = %d (%s), want 400 from the bot", path, response.StatusCode, body)
			}
		})
	}
}

// TestBotAPIAliasThroughGateway keeps the /.proxy/api prefix working, which is
// the shape Discord's proxy leaves when the mapping target is /api.
func TestBotAPIAliasThroughGateway(t *testing.T) {
	stack := startStack(t)

	for _, path := range []string{"/api/lobby/streams", "/.proxy/api/lobby/streams"} {
		response, err := http.Get("http://" + stack.gatewayAddr + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d (%s), want 200", path, response.StatusCode, body)
		}
		if strings.TrimSpace(string(body)) != "[]" {
			t.Fatalf("%s body = %q, want []", path, body)
		}
	}
}

// TestLobbyAPIThroughGateway checks the gateway's /api proxy and the bot's
// lobby endpoints, which is the other half of the local stack.
func TestLobbyAPIThroughGateway(t *testing.T) {
	stack := startStack(t)
	base := "http://" + stack.gatewayAddr

	response, err := http.Get(base + "/api/lobby/streams")
	if err != nil {
		t.Fatalf("list streams: %v", err)
	}
	emptyBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read streams: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list streams status = %d", response.StatusCode)
	}
	// An empty lobby must be [] rather than null so a viewer can iterate it.
	if strings.TrimSpace(string(emptyBody)) != "[]" {
		t.Fatalf("empty lobby = %q, want []", emptyBody)
	}

	stream := map[string]any{
		"room": "room-3", "token": "viewer-token", "name": "Tester", "iceServers": []string{"stun:example.org"},
	}
	body, _ := json.Marshal(stream)
	response, err = http.Post(base+"/api/lobby/streams", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("put stream: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("put stream status = %d", response.StatusCode)
	}

	response, err = http.Get(base + "/api/lobby/streams")
	if err != nil {
		t.Fatalf("list streams: %v", err)
	}
	defer response.Body.Close()
	var streams []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if len(streams) != 1 || streams[0]["room"] != "room-3" {
		t.Fatalf("streams = %+v, want room-3", streams)
	}
}

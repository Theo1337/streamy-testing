package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"streamy/internal/activity/protocol"
	"streamy/internal/activity/room"
)

type Server struct {
	logger     *slog.Logger
	publishTok string
	maxViewers int

	mu         sync.Mutex
	rooms      map[string]*room.Room
	httpServer *http.Server

	upgrader websocket.Upgrader
}

func NewServer(logger *slog.Logger, publishToken string, maxViewers int) *Server {
	return &Server{
		logger:     logger,
		publishTok: publishToken,
		maxViewers: maxViewers,
		rooms:      make(map[string]*room.Room),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/publish/{room}", s.handlePublisher)
	mux.HandleFunc("GET /v1/watch/{room}", s.handleViewer)
	// Discord strips the URL-mapping prefix, so a `/.proxy/relay/...` viewer
	// reaches `/v1/watch/...`. Accept the prefixed paths too.
	mux.HandleFunc("GET /relay/v1/publish/{room}", s.handlePublisher)
	mux.HandleFunc("GET /relay/v1/watch/{room}", s.handleViewer)
	return mux
}

// joinRoom adds a client to a room while the room registry is locked. Doing
// the lookup and the join as one step means dropRoom cannot delete a room
// between them and leave the new client attached to a room nobody else can
// find. The lock order is always Server.mu then Room.mu.
func (s *Server) joinRoom(id string, join func(*room.Room) bool) (*room.Room, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.rooms[id]
	if r == nil {
		r = room.NewRoom(id)
		s.rooms[id] = r
	}
	if !join(r) {
		if s.rooms[id] == r && r.Empty() {
			delete(s.rooms, id)
		}
		return r, false
	}
	return r, true
}

func (s *Server) dropRoom(id string, r *room.Room) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rooms[id] == r && r.Empty() {
		delete(s.rooms, id)
	}
}

func (s *Server) handlePublisher(w http.ResponseWriter, req *http.Request) {
	s.handle(w, req, "publisher")
}

func (s *Server) handleViewer(w http.ResponseWriter, req *http.Request) {
	s.handle(w, req, "viewer")
}

func (s *Server) handle(w http.ResponseWriter, req *http.Request, role string) {
	roomID := strings.TrimSpace(req.PathValue("room"))
	if !validRoomID(roomID) {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}
	conn, err := s.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(protocol.MaxMediaBytes + protocol.MediaHeaderSize)
	defer conn.Close()

	client := &room.RelayClient{
		Conn: conn,
		Role: role,
		Send: make(chan room.Outbound, room.ViewerQueueDepth),
		Done: make(chan struct{}),
	}

	hello, err := readHello(conn)
	if err != nil {
		writeJSONNow(conn, errorMessage("bad-message", err.Error()))
		conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "bad hello"),
			time.Now().Add(time.Second))
		return
	}
	if hello.Version != protocol.ProtocolVersion {
		writeJSONNow(conn, errorMessage("unsupported-version", "relay protocol version is 1"))
		client.Close(websocket.CloseProtocolError, "unsupported version")
		return
	}
	if hello.Role != role {
		writeJSONNow(conn, errorMessage("bad-message", "role does not match endpoint"))
		client.Close(websocket.ClosePolicyViolation, "wrong role")
		return
	}

	conn.SetReadDeadline(time.Now().Add(protocol.ReadDeadline))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(protocol.ReadDeadline))
	})

	switch role {
	case "publisher":
		if s.publishTok != "" && !constantTimeEqual(hello.Token, s.publishTok) {
			writeJSONNow(conn, errorMessage("unauthorized", "invalid publisher token"))
			client.Close(protocol.CloseUnauthorized, "unauthorized")
			return
		}
		r, ok := s.joinRoom(roomID, func(r *room.Room) bool { return r.AddPublisher(client) })
		client.Room = r
		if !ok {
			writeJSONNow(conn, errorMessage("room-in-use", "a publisher is already connected"))
			client.Close(protocol.CloseRoomInUse, "room in use")
			s.dropRoom(roomID, r)
			return
		}
		defer func() {
			r.RemovePublisher(client)
			r.CloseViewers(protocol.CloseHostGone, "publisher disconnected")
			s.dropRoom(roomID, r)
			s.logger.Info("publisher left", "room", roomID)
		}()
		client.SendJSON(map[string]any{
			"v": protocol.ProtocolVersion, "type": "ready", "room": roomID, "role": "publisher",
		})
		s.logger.Info("publisher connected", "room", roomID, "viewers", r.ViewerCount())

	case "viewer":
		client.SetNeedsKeyframe(true)
		r, ok := s.joinRoom(roomID, func(r *room.Room) bool { return r.AddViewer(client, s.maxViewers) })
		client.Room = r
		if !ok {
			writeJSONNow(conn, errorMessage("room-full", "viewer limit reached"))
			client.Close(protocol.CloseRoomFull, "room full")
			s.dropRoom(roomID, r)
			return
		}
		defer func() {
			r.RemoveViewer(client)
			s.dropRoom(roomID, r)
			s.logger.Debug("viewer left", "room", roomID, "viewers", r.ViewerCount())
		}()
		client.SendJSON(map[string]any{
			"v": protocol.ProtocolVersion, "type": "ready", "room": roomID, "role": "viewer",
		})
		r.SendCachedKeyframe(client, uint64(time.Now().UnixMilli()))
		s.logger.Info("viewer connected", "room", roomID, "viewers", r.ViewerCount())
	}

	go client.Writer()
	s.readLoop(client)
}

func (s *Server) readLoop(c *room.RelayClient) {
	defer c.Close(websocket.CloseNormalClosure, "leaving")
	for {
		messageType, data, err := c.Conn.ReadMessage()
		if err != nil {
			return
		}
		c.Conn.SetReadDeadline(time.Now().Add(protocol.ReadDeadline))
		if messageType == websocket.TextMessage {
			var control room.ControlMessage
			if err := json.Unmarshal(data, &control); err != nil {
				continue
			}
			if control.Type == "ping" {
				c.SendJSON(map[string]any{"v": protocol.ProtocolVersion, "type": "pong", "t": control.Time})
			}
			continue
		}
		if messageType != websocket.BinaryMessage || c.Role != "publisher" {
			continue
		}
		frame, err := protocol.ParseMediaFrame(data)
		if err != nil || len(frame.Payload) == 0 {
			continue
		}
		c.Room.Publish(frame, data)
	}
}

type helloMessage struct {
	Version int    `json:"v"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Token   string `json:"token"`
	Time    int64  `json:"t"`
}

func readHello(conn *websocket.Conn) (helloMessage, error) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return helloMessage{}, err
	}
	if messageType != websocket.TextMessage || len(data) > protocol.MaxControlBytes {
		return helloMessage{}, errors.New("first message must be a small JSON hello")
	}
	var hello helloMessage
	if err := json.Unmarshal(data, &hello); err != nil {
		return helloMessage{}, errors.New("hello is not valid JSON")
	}
	if hello.Type != "hello" {
		return helloMessage{}, errors.New("first message must have type hello")
	}
	conn.SetReadDeadline(time.Time{})
	return hello, nil
}

func errorMessage(code, message string) map[string]any {
	return map[string]any{
		"v": protocol.ProtocolVersion, "type": "error", "code": code, "message": message,
	}
}

func writeJSONNow(conn *websocket.Conn, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.WriteMessage(websocket.TextMessage, data)
}

func validRoomID(id string) bool {
	if len(id) < protocol.MinRoomIDLength || len(id) > protocol.MaxRoomIDLength {
		return false
	}
	for _, ch := range id {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			continue
		}
		return false
	}
	return true
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range len(a) {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (s *Server) Run(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("relay listen %s: %w", addr, err)
	}
	return s.Serve(ctx, listener)
}

// Serve runs the relay on an already-bound listener. Binding separately lets
// the caller fail fast on a busy port and learn the real address when :0 was
// requested.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           s.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.mu.Lock()
	s.httpServer = httpServer
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("streamy relay listening", "addr", listener.Addr().String(), "max_viewers", s.maxViewers)
		errCh <- httpServer.Serve(listener)
	}()

	go s.logStats(ctx)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpServer != nil {
		return s.httpServer.Addr
	}
	return ""
}

func (s *Server) logStats(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			rooms := make([]*room.Room, 0, len(s.rooms))
			for _, r := range s.rooms {
				rooms = append(rooms, r)
			}
			s.mu.Unlock()
			for _, r := range rooms {
				viewers, bytesIn, bytesOut := r.Stats()
				if viewers == 0 && bytesIn == 0 && bytesOut == 0 {
					continue
				}
				s.logger.Info("relay bandwidth",
					"room", r.ID(),
					"viewers", viewers,
					"publisher_kbps", float64(bytesIn)*8/10/1000,
					"viewer_egress_kbps", float64(bytesOut)*8/10/1000,
				)
			}
		}
	}
}

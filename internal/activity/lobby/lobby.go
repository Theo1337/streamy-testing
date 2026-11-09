package lobby

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	StreamTTL     = 90 * time.Second
	OAuthStateTTL = 5 * time.Minute
)

var (
	ErrInvalidStream  = errors.New("invalid stream data")
	ErrInvalidState   = errors.New("invalid OAuth state")
	ErrStateExpired   = errors.New("OAuth state expired")
	ErrStreamNotFound = errors.New("stream not found")
)

type Stream struct {
	Room        string   `json:"room"`
	Token       string   `json:"token"`
	Name        string   `json:"name"`
	Avatar      string   `json:"avatar"`
	ViewerCount int      `json:"viewerCount"`
	ICEServers  []string `json:"iceServers"`
	UpdatedAt   int64    `json:"updatedAt"`
}

type OAuthState struct {
	Callback    string `json:"callback"`
	ClientState string `json:"clientState"`
	CreatedAt   int64  `json:"createdAt"`
}

type TunnelInfo struct {
	Host      string `json:"host"`
	UpdatedAt int64  `json:"updatedAt"`
}

type Lobby struct {
	mu          sync.RWMutex
	streams     map[string]Stream
	oauthStates map[string]OAuthState
	tunnel      *TunnelInfo
}

func NewLobby() *Lobby {
	return &Lobby{
		streams:     make(map[string]Stream),
		oauthStates: make(map[string]OAuthState),
	}
}

func (l *Lobby) GetStreams() []Stream {
	l.mu.RLock()
	defer l.mu.RUnlock()

	now := time.Now().UnixMilli()
	result := make([]Stream, 0)
	for _, stream := range l.streams {
		if now-stream.UpdatedAt < StreamTTL.Milliseconds() {
			result = append(result, stream)
		}
	}
	return result
}

func (l *Lobby) PutStream(stream Stream) error {
	if stream.Room == "" || stream.Token == "" || stream.Name == "" {
		return ErrInvalidStream
	}
	if len(stream.ICEServers) > 16 {
		return ErrInvalidStream
	}
	stream.UpdatedAt = time.Now().UnixMilli()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.streams[stream.Room] = stream
	return nil
}

func (l *Lobby) DeleteStream(room string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.streams, room)
}

func (l *Lobby) StoreOAuthState(state, callback, clientState string) error {
	if len(state) < 32 || len(clientState) < 32 || callback == "" {
		return ErrInvalidState
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.oauthStates[state] = OAuthState{
		Callback:    callback,
		ClientState: clientState,
		CreatedAt:   time.Now().UnixMilli(),
	}
	return nil
}

func (l *Lobby) ConsumeOAuthState(state string) (OAuthState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	oauth, ok := l.oauthStates[state]
	if !ok {
		return OAuthState{}, ErrStateExpired
	}
	delete(l.oauthStates, state)

	if time.Now().UnixMilli()-oauth.CreatedAt > OAuthStateTTL.Milliseconds() {
		return OAuthState{}, ErrStateExpired
	}
	return oauth, nil
}

func (l *Lobby) GetTunnel() *TunnelInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.tunnel == nil {
		return nil
	}
	return &TunnelInfo{Host: l.tunnel.Host, UpdatedAt: l.tunnel.UpdatedAt}
}

func (l *Lobby) SetTunnel(host string) error {
	if host == "" {
		return errors.New("invalid host")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tunnel = &TunnelInfo{
		Host:      host,
		UpdatedAt: time.Now().UnixMilli(),
	}
	return nil
}

func (l *Lobby) DeleteTunnel() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tunnel = nil
}

func (l *Lobby) Cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now().UnixMilli()
	for room, stream := range l.streams {
		if now-stream.UpdatedAt > StreamTTL.Milliseconds() {
			delete(l.streams, room)
		}
	}
	for state, oauth := range l.oauthStates {
		if now-oauth.CreatedAt > OAuthStateTTL.Milliseconds() {
			delete(l.oauthStates, state)
		}
	}
}

// MarshalJSON for HTTP responses
func (s Stream) MarshalJSON() ([]byte, error) {
	type Alias Stream
	return json.Marshal(struct {
		Alias
		ICEServers []string `json:"iceServers"`
	}{
		Alias:      Alias(s),
		ICEServers: s.ICEServers,
	})
}

package bot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"streamy/internal/activity/identity"
	"streamy/internal/activity/lobby"
	"streamy/internal/activity/protocol"
)

const (
	MaxTunnelRetries = 5
	OAuthStateBytes  = 32
)

var (
	ErrMissingConfig     = errors.New("missing required config")
	ErrTunnelTimeout     = errors.New("tunnel registration timeout")
	ErrTunnelUnreachable = errors.New("tunnel unreachable")
)

type Config struct {
	DiscordClientID     string
	DiscordClientSecret string
	IdentitySecret      string
	PublishToken        string
	RelayAddr           string
	GatewayAddr         string
	BotAddr             string
	PublicHost          string
	MaxViewers          int
	ICEServers          []string
}

type Bot struct {
	logger     *slog.Logger
	cfg        Config
	lobby      *lobby.Lobby
	httpServer *http.Server

	mu        sync.Mutex
	tunnelURL string
	quitCh    chan struct{}
	quitOnce  sync.Once
}

func NewBot(logger *slog.Logger, cfg Config) (*Bot, error) {
	if cfg.RelayAddr == "" {
		return nil, fmt.Errorf("%w: relay address required", ErrMissingConfig)
	}
	if cfg.GatewayAddr == "" {
		return nil, fmt.Errorf("%w: gateway address required", ErrMissingConfig)
	}
	if cfg.BotAddr == "" {
		return nil, fmt.Errorf("%w: bot address required", ErrMissingConfig)
	}
	// Discord sign-in is optional: the relay, the lobby API and the viewer work
	// without it, which is what makes a local Activity stack runnable with no
	// Discord application at all. Only the OAuth endpoints need these.
	if cfg.IdentitySecret != "" {
		if _, err := identity.SigningKey(cfg.IdentitySecret); err != nil {
			return nil, err
		}
	}
	if cfg.MaxViewers == 0 {
		cfg.MaxViewers = protocol.DefaultMaxViewers
	}
	if cfg.ICEServers == nil {
		cfg.ICEServers = []string{"stun:stun.l.google.com:19302"}
	}

	bot := &Bot{
		logger: logger,
		cfg:    cfg,
		lobby:  lobby.NewLobby(),
		quitCh: make(chan struct{}),
	}
	if !bot.authConfigured() && logger != nil {
		logger.Warn("Discord sign-in is not configured; /api/auth/* will return 503",
			"discord_client_id", cfg.DiscordClientID != "",
			"discord_client_secret", cfg.DiscordClientSecret != "",
			"identity_secret", cfg.IdentitySecret != "")
	}
	return bot, nil
}

// authConfigured reports whether the Discord OAuth endpoints have the three
// values they need: client ID, client secret and the identity signing key.
func (b *Bot) authConfigured() bool {
	return b.cfg.DiscordClientID != "" &&
		b.cfg.DiscordClientSecret != "" &&
		b.cfg.IdentitySecret != ""
}

func (b *Bot) Quit() <-chan struct{} {
	return b.quitCh
}

func (b *Bot) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/auth/discord/start", b.handleAuthStart)
	mux.HandleFunc("GET /api/auth/discord/callback", b.handleAuthCallback)
	mux.HandleFunc("GET /api/lobby/streams", b.handleLobbyStreams)
	mux.HandleFunc("POST /api/lobby/streams", b.handleLobbyPutStream)
	mux.HandleFunc("DELETE /api/lobby/streams/{room}", b.handleLobbyDeleteStream)
	mux.HandleFunc("POST /api/notify", b.handleNotify)
	mux.HandleFunc("POST /api/tunnel", b.handleTunnelRegister)
	mux.HandleFunc("DELETE /api/tunnel", b.handleTunnelDelete)
	mux.HandleFunc("POST /api/quit", b.handleQuit)
	// Token exchange for Discord Activity SDK: receives OAuth code, returns access_token
	mux.HandleFunc("POST /api/token", b.handleTokenExchange)
	mux.HandleFunc("POST /token", b.handleTokenExchange)
	mux.HandleFunc("GET /api/config", b.handleConfig)
	return withCORS(mux)
}

// withCORS lets the Activity viewer call the bot directly from a Discord
// origin. Inside a real Activity the /token call is proxied by Discord and is
// same-origin, but the same page opened locally (or served by the Worker)
// needs these headers.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (b *Bot) handleAuthStart(w http.ResponseWriter, req *http.Request) {
	if !b.authConfigured() {
		http.Error(w, "Discord sign-in is not configured on this bot", http.StatusServiceUnavailable)
		return
	}
	callback := req.URL.Query().Get("callback")
	clientState := req.URL.Query().Get("state")
	if !identity.ValidLoopbackCallback(callback) {
		http.Error(w, "invalid callback URL", http.StatusBadRequest)
		return
	}
	if len(clientState) < 32 {
		http.Error(w, "state parameter too short", http.StatusBadRequest)
		return
	}

	stateBytes := make([]byte, OAuthStateBytes)
	if _, err := rand.Read(stateBytes); err != nil {
		http.Error(w, "failed to generate state", http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	if err := b.lobby.StoreOAuthState(state, callback, clientState); err != nil {
		http.Error(w, "failed to store state", http.StatusInternalServerError)
		return
	}

	discordAuthURL := fmt.Sprintf(
		"https://discord.com/api/oauth2/authorize?client_id=%s&redirect_uri=%s&response_type=code&scope=identify&state=%s&prompt=consent",
		url.QueryEscape(b.cfg.DiscordClientID),
		url.QueryEscape(callback),
		url.QueryEscape(state),
	)
	http.Redirect(w, req, discordAuthURL, http.StatusFound)
}

func (b *Bot) handleAuthCallback(w http.ResponseWriter, req *http.Request) {
	if !b.authConfigured() {
		http.Error(w, "Discord sign-in is not configured on this bot", http.StatusServiceUnavailable)
		return
	}
	code := req.URL.Query().Get("code")
	state := req.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	oauthState, err := b.lobby.ConsumeOAuthState(state)
	if err != nil {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}

	tokenURL := "https://discord.com/api/oauth2/token"
	data := url.Values{
		"client_id":     {b.cfg.DiscordClientID},
		"client_secret": {b.cfg.DiscordClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {oauthState.Callback},
	}

	tokenResp, err := http.PostForm(tokenURL, data)
	if err != nil {
		http.Error(w, "token request failed", http.StatusInternalServerError)
		return
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	var tokenData struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		http.Error(w, "invalid token response", http.StatusInternalServerError)
		return
	}

	userReq, _ := http.NewRequest("GET", "https://discord.com/api/users/@me", nil)
	userReq.Header.Set("Authorization", "Bearer "+tokenData.AccessToken)
	userResp, err := http.DefaultClient.Do(userReq)
	if err != nil {
		http.Error(w, "user fetch failed", http.StatusInternalServerError)
		return
	}
	defer userResp.Body.Close()

	if userResp.StatusCode != http.StatusOK {
		http.Error(w, "user fetch failed", http.StatusInternalServerError)
		return
	}

	var user identity.DiscordUser
	if err := json.NewDecoder(userResp.Body).Decode(&user); err != nil {
		http.Error(w, "invalid user response", http.StatusInternalServerError)
		return
	}

	identityToken, err := identity.IssueIdentityToken(user, b.cfg.IdentitySecret)
	if err != nil {
		http.Error(w, "identity token generation failed", http.StatusInternalServerError)
		return
	}

	redirectURL := oauthState.Callback + "?token=" + url.QueryEscape(identityToken) + "&state=" + url.QueryEscape(oauthState.ClientState)
	http.Redirect(w, req, redirectURL, http.StatusFound)
}

// handleTokenExchange exchanges a Discord OAuth authorization code for an access_token.
// Used by the Discord Activity SDK flow: the viewer gets a code via sdk.commands.authorize(),
// then POSTs it here to get the access_token for sdk.commands.authenticate().
func (b *Bot) handleTokenExchange(w http.ResponseWriter, req *http.Request) {
	if !b.authConfigured() {
		http.Error(w, "Discord sign-in is not configured on this bot", http.StatusServiceUnavailable)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		http.Error(w, "code required", http.StatusBadRequest)
		return
	}

	tokenURL := "https://discord.com/api/oauth2/token"
	// The Activity SDK flow does not use a redirect URI: the code comes from
	// sdk.commands.authorize() inside the iframe. Sending a made-up
	// redirect_uri makes Discord answer invalid_grant.
	data := url.Values{
		"client_id":     {b.cfg.DiscordClientID},
		"client_secret": {b.cfg.DiscordClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
	}

	tokenResp, err := http.PostForm(tokenURL, data)
	if err != nil {
		http.Error(w, "token request failed", http.StatusInternalServerError)
		return
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(tokenResp.Body, 512))
		b.logger.Error("token exchange failed", "status", tokenResp.StatusCode, "body", string(detail))
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}

	var tokenData struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		http.Error(w, "invalid token response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Discord's SDK expects expires_in as a number, not a string.
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": tokenData.AccessToken,
		"token_type":   tokenData.TokenType,
		"expires_in":   tokenData.ExpiresIn,
		"scope":        tokenData.Scope,
	})
}

func (b *Bot) handleLobbyStreams(w http.ResponseWriter, _ *http.Request) {
	streams := b.lobby.GetStreams()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(streams)
}

func (b *Bot) handleLobbyPutStream(w http.ResponseWriter, req *http.Request) {
	var stream lobby.Stream
	if err := json.NewDecoder(req.Body).Decode(&stream); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	stream.ICEServers = b.cfg.ICEServers

	if err := b.lobby.PutStream(stream); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (b *Bot) handleLobbyDeleteStream(w http.ResponseWriter, req *http.Request) {
	roomID := strings.TrimSpace(req.PathValue("room"))
	if roomID == "" {
		http.Error(w, "room required", http.StatusBadRequest)
		return
	}
	b.lobby.DeleteStream(roomID)
	w.WriteHeader(http.StatusNoContent)
}

type notifyPayload struct {
	Room     string `json:"room"`
	Username string `json:"username"`
	Avatar   string `json:"avatar"`
	Message  string `json:"message"`
}

func (b *Bot) handleNotify(w http.ResponseWriter, req *http.Request) {
	var p notifyPayload
	if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if p.Room == "" || p.Message == "" {
		http.Error(w, "room and message required", http.StatusBadRequest)
		return
	}
	// TODO: Send notification to publisher via relay
	b.logger.Info("notify", "room", p.Room, "from", p.Username)
	w.WriteHeader(http.StatusAccepted)
}

func (b *Bot) handleTunnelRegister(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Host == "" {
		http.Error(w, "host required", http.StatusBadRequest)
		return
	}
	if err := b.SetTunnelURL(body.Host); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// SetTunnelURL sets the tunnel URL programmatically (e.g., from cloudflared).
// The host should be just the hostname (e.g., "abc123.trycloudflare.com").
func (b *Bot) SetTunnelURL(host string) error {
	if host == "" {
		return errors.New("host required")
	}
	if err := b.lobby.SetTunnel(host); err != nil {
		return err
	}
	b.mu.Lock()
	b.tunnelURL = "https://" + host
	b.mu.Unlock()
	b.logger.Info("tunnel registered", "url", b.tunnelURL)
	return nil
}

// DeleteTunnel clears the tunnel URL.
func (b *Bot) DeleteTunnel() {
	b.lobby.DeleteTunnel()
	b.mu.Lock()
	b.tunnelURL = ""
	b.mu.Unlock()
	b.logger.Info("tunnel cleared")
}

func (b *Bot) handleTunnelDelete(w http.ResponseWriter, _ *http.Request) {
	b.DeleteTunnel()
	w.WriteHeader(http.StatusOK)
}

func (b *Bot) handleQuit(w http.ResponseWriter, _ *http.Request) {
	b.quitOnce.Do(func() {
		close(b.quitCh)
	})
	w.WriteHeader(http.StatusOK)
}

func (b *Bot) Run(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("bot listen %s: %w", addr, err)
	}
	return b.Serve(ctx, listener)
}

// Serve runs the bot on an already-bound listener.
func (b *Bot) Serve(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           b.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	b.mu.Lock()
	b.httpServer = httpServer
	b.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		b.logger.Info("streamy bot listening", "addr", listener.Addr().String())
		errCh <- httpServer.Serve(listener)
	}()

	go b.cleanupLoop(ctx)
	go b.waitForQuit(ctx)

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

func (b *Bot) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.lobby.Cleanup()
		}
	}
}

func (b *Bot) waitForQuit(ctx context.Context) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ctx.Done():
	case <-sigCh:
	case <-b.quitCh:
	}
	b.quitOnce.Do(func() {
		close(b.quitCh)
	})
}

func (b *Bot) TunnelURL() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tunnelURL
}

func (b *Bot) PublicURL(path string) string {
	if tunnel := b.TunnelURL(); tunnel != "" {
		return tunnel + path
	}
	host := b.cfg.PublicHost
	if host == "" {
		host, _, _ = net.SplitHostPort(b.cfg.GatewayAddr)
		if host == "" {
			host = "localhost"
		}
	}
	return "http://" + host + path
}

func (b *Bot) RelayWSURL(path string) string {
	host := b.cfg.PublicHost
	if host == "" {
		host, _, _ = net.SplitHostPort(b.cfg.GatewayAddr)
		if host == "" {
			host = "localhost"
		}
	}
	return "ws://" + host + "/.proxy/relay" + path
}

func (b *Bot) Addr() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.httpServer != nil {
		return b.httpServer.Addr
	}
	return b.cfg.BotAddr
}

func (b *Bot) DiscordClientID() string {
	return b.cfg.DiscordClientID
}

func (b *Bot) handleConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"discordClientID": b.cfg.DiscordClientID,
		"tokenURL":        "/.proxy/api/token",
	})
}

// Package bot is the local Discord bot that posts the public viewer link.
//
// It is the Go replacement for the Node/discord.js bot in stream-site/bot. The
// HTTP contract is unchanged: the desktop app POSTs to /api/notify with a
// bearer key, and the bot posts or deletes a plain message with the viewer
// link. It listens on loopback only.
package bot

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	// DefaultPort is the port the desktop app expects (stream.discord.notifyURL).
	DefaultPort = 8791

	maxBodyBytes     = 16 * 1024
	maxStreamerRunes = 80
	defaultViewerURL = "https://streamy-viewer.pages.dev/"
)

var (
	channelIDPattern = regexp.MustCompile(`^\d{5,32}$`)
	liveLinkPattern  = regexp.MustCompile(`(?i)is live\. Watch here: https?://`)
	markdownEscaper  = strings.NewReplacer(
		`\`, `\\`,
		"*", `\*`,
		"_", `\_`,
		"`", "\\`",
		"~", `\~`,
	)
)

// Config is everything the bot needs to run.
type Config struct {
	Token          string
	APIKey         string
	WatchChannelID string
	ViewerURL      string
	Port           int
	GuildID        string
}

// ConfigFromEnv reads the bot settings from the process environment. The .env
// file (see LoadConfig) is loaded into the environment first.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Token:          strings.TrimSpace(os.Getenv("DISCORD_TOKEN")),
		APIKey:         strings.TrimSpace(os.Getenv("STREAMY_API_KEY")),
		WatchChannelID: strings.TrimSpace(os.Getenv("WATCH_CHANNEL_ID")),
		ViewerURL:      strings.TrimSpace(os.Getenv("VIEWER_URL")),
		Port:           DefaultPort,
		GuildID:        strings.TrimSpace(os.Getenv("GUILD_ID")),
	}
	if cfg.ViewerURL == "" {
		cfg.ViewerURL = defaultViewerURL
	}
	if raw := strings.TrimSpace(os.Getenv("PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("PORT: %q is not a valid port", raw)
		}
		cfg.Port = port
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Token == "" {
		return errors.New("DISCORD_TOKEN is required")
	}
	if c.APIKey == "" {
		return errors.New("STREAMY_API_KEY is required")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d is out of range", c.Port)
	}
	if c.GuildID != "" && !channelIDPattern.MatchString(c.GuildID) {
		return errors.New("guildID: must be a Discord snowflake")
	}
	return nil
}

// LoadConfig finds and loads the .env file, then builds a Config. The returned
// path is empty when no file was found and the environment already had the
// required values.
func LoadConfig(explicitEnv string) (Config, string, error) {
	path := FindEnvFile(explicitEnv)
	if path != "" {
		if err := LoadEnvFile(path); err != nil {
			return Config{}, "", fmt.Errorf("load %s: %w", path, err)
		}
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		if path == "" {
			return Config{}, "", fmt.Errorf("%w (no .env file found; set DISCORD_TOKEN and STREAMY_API_KEY)", err)
		}
		return Config{}, path, err
	}
	return cfg, path, nil
}

// discordClient is the slice of discordgo the bot uses. Keeping it as an
// interface lets the HTTP API be tested without a Discord connection.
type discordClient interface {
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
	ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageDelete(channelID, messageID string, options ...discordgo.RequestOption) error
}

// Run connects to Discord, serves the local HTTP API and blocks until ctx is
// cancelled or /api/quit is called.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if cfg.ViewerURL == "" {
		cfg.ViewerURL = defaultViewerURL
	}

	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return fmt.Errorf("create Discord session: %w", err)
	}
	// Guilds and voice states are needed to resolve the voice channel a user is in.
	session.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates
	session.LogLevel = discordgo.LogWarning

	if err := session.Open(); err != nil {
		return fmt.Errorf("Discord login: %w", err)
	}
	defer session.Close()

	if session.State.User == nil {
		return errors.New("Discord login returned no user")
	}

	b := newBot(session, session.State.User.ID, cfg, logger)
	server := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		Handler:           b.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()

	logger.Info("Discord bot ready",
		"user", session.State.User.String(),
		"port", cfg.Port,
		"viewerURL", cfg.ViewerURL,
	)

	select {
	case <-ctx.Done():
	case <-b.quit:
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("bot HTTP server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("bot HTTP shutdown", "error", err)
	}
	return nil
}

type bot struct {
	client discordClient
	botID  string
	cfg    Config
	logger *slog.Logger

	// notifyMu serialises start/stop so two concurrent requests cannot both
	// post a link. The Node version got this for free from its event loop.
	notifyMu sync.Mutex

	mu   sync.Mutex
	live map[string]string // channelID -> messageID

	quit     chan struct{}
	quitOnce sync.Once

	}

func newBot(client discordClient, botID string, cfg Config, logger *slog.Logger) *bot {
	return &bot{
		client: client,
		botID:  botID,
		cfg:    cfg,
		logger: logger,
		live:   make(map[string]string),
		quit:   make(chan struct{}),
	}
}

func (b *bot) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", b.handleHealth)
	mux.HandleFunc("/api/notify", b.handleNotify)
	mux.HandleFunc("/api/quit", b.handleQuit)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	})
	return withCORS(mux)
}

func (b *bot) shutdown() {
	b.quitOnce.Do(func() { close(b.quit) })
}

func (b *bot) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (b *bot) handleQuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !b.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	b.shutdown()
}

type notifyRequest struct {
	Action    string `json:"action"`
	ChannelID string `json:"channelId"`
	Streamer  string `json:"streamer"`
	URL       string `json:"url"`
	Live      *bool  `json:"live"`
}

func (b *bot) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !b.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	b.notifyMu.Lock()
	defer b.notifyMu.Unlock()

	var body notifyRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}

	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action == "" {
		action = "start"
	}
	requestedChannelID := strings.TrimSpace(body.ChannelID)
	if requestedChannelID == "" {
		requestedChannelID = b.cfg.WatchChannelID
	}

	if action == "stop" || action == "end" || (body.Live != nil && !*body.Live) {
		channels := append([]string{requestedChannelID, b.cfg.WatchChannelID}, b.liveKeys()...)
		deleted, err := b.removeLiveMessages(channels...)
		if err != nil {
			b.logger.Warn("could not remove the Discord live link", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
		return
	}

	if !channelIDPattern.MatchString(requestedChannelID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "channelId is required"})
		return
	}
	target, err := b.resolveTargetChannel(requestedChannelID)
	if err != nil {
		b.logger.Warn("could not fetch the Discord channel", "channelId", requestedChannelID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "channel not found or not text based"})
		return
	}
	link := b.viewerURL(body.URL)
	if link == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "url must be an http(s) viewer link"})
		return
	}

	// One link at a time: remove the previous one before posting, including
	// the configured fallback channel and any channel we already tracked.
	channels := append([]string{target.ID, requestedChannelID, b.cfg.WatchChannelID}, b.liveKeys()...)
	if _, err := b.removeLiveMessages(channels...); err != nil {
		b.logger.Warn("could not remove the previous Discord live link", "error", err)
	}

	streamer := truncateRunes(strings.TrimSpace(body.Streamer), maxStreamerRunes)
	sent, err := b.postLiveMessage(target.ID, link, streamer)
	if err != nil {
		b.logger.Warn("could not post the Discord live link", "channelId", target.ID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
		return
	}
	b.setLive(target.ID, sent.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"messageId": sent.ID,
		"channelId": target.ID,
		"url":       link,
	})
}

func (b *bot) resolveTargetChannel(requestedChannelID string) (*discordgo.Channel, error) {
	for _, candidate := range []string{requestedChannelID, b.cfg.WatchChannelID} {
		channelID := strings.TrimSpace(candidate)
		if !channelIDPattern.MatchString(channelID) {
			continue
		}
		channel, err := b.client.Channel(channelID)
		if err != nil {
			// The Node bot treated a failed fetch as "not found"; keep the
			// same response so the desktop app sees a stable 404.
			b.logger.Debug("could not fetch the Discord channel", "channelId", channelID, "error", err)
			continue
		}
		if channel != nil && isTextBased(channel) {
			return channel, nil
		}
		// If the channel is a voice channel (or stage voice), return it as-is
		// so the app doesn't get a 404 for a valid voice channel ID.
		if channel != nil && (channel.Type == discordgo.ChannelTypeGuildVoice || channel.Type == discordgo.ChannelTypeGuildStageVoice) {
			return channel, nil
		}
	}
	return nil, nil
}

func (b *bot) postLiveMessage(channelID, link, streamer string) (*discordgo.Message, error) {
	content := "Streamy is live. Watch here: " + link
	if streamer != "" {
		content = "**" + escapeMarkdown(streamer) + "** is live. Watch here: " + link
	}
	return b.client.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content,
		// An empty, non-nil Parse sends "parse":[] and disables every mention,
		// matching discord.js's allowedMentions: { parse: [] }.
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	})
}

// removeLiveMessages deletes the tracked message and any recent message from
// this bot that looks like a Streamy link. Scanning history means a bot restart
// cannot leave a stale link behind.
func (b *bot) removeLiveMessages(channelIDs ...string) (int, error) {
	deleted := 0
	seen := make(map[string]bool)
	for _, raw := range channelIDs {
		channelID := strings.TrimSpace(raw)
		if !channelIDPattern.MatchString(channelID) || seen[channelID] {
			continue
		}
		seen[channelID] = true

		channel, err := b.client.Channel(channelID)
		if err != nil || channel == nil || !isTextBased(channel) {
			continue
		}

		candidates := make(map[string]bool)
		if tracked := b.takeLive(channelID); tracked != "" {
			candidates[tracked] = true
		}
		recent, err := b.client.ChannelMessages(channelID, 50, "", "", "")
		if err == nil {
			for _, message := range recent {
				if message.Author != nil && message.Author.ID == b.botID && isLiveMessage(message.Content) {
					candidates[message.ID] = true
				}
			}
		}

		for messageID := range candidates {
			message, err := b.messageFrom(channelID, messageID, recent)
			if err != nil || message == nil || message.Author == nil || message.Author.ID != b.botID {
				continue
			}
			if err := b.client.ChannelMessageDelete(channelID, messageID); err == nil {
				deleted++
			}
			// Missing "Manage messages" permission is not fatal for stopping.
		}
	}
	return deleted, nil
}

func (b *bot) messageFrom(channelID, messageID string, recent []*discordgo.Message) (*discordgo.Message, error) {
	for _, message := range recent {
		if message.ID == messageID {
			return message, nil
		}
	}
	return b.client.ChannelMessage(channelID, messageID)
}

func (b *bot) viewerURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = strings.TrimSpace(b.cfg.ViewerURL)
	}
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.String()
}

func (b *bot) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	token := ""
	if strings.HasPrefix(header, "Bearer ") {
		token = header[len("Bearer "):]
	}
	return equalStrings(token, b.cfg.APIKey)
}

func (b *bot) setLive(channelID, messageID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.live[channelID] = messageID
}

func (b *bot) takeLive(channelID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	messageID := b.live[channelID]
	delete(b.live, channelID)
	return messageID
}

func (b *bot) liveKeys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.live))
	for channelID := range b.live {
		keys = append(keys, channelID)
	}
	return keys
}

func isTextBased(channel *discordgo.Channel) bool {
	switch channel.Type {
	case discordgo.ChannelTypeGuildText,
		discordgo.ChannelTypeGuildNews,
		discordgo.ChannelTypeGuildVoice,
		discordgo.ChannelTypeGuildStageVoice,
		discordgo.ChannelTypeGuildPublicThread,
		discordgo.ChannelTypeGuildPrivateThread,
		discordgo.ChannelTypeGuildNewsThread:
		return true
	}
	return false
}

func isLiveMessage(content string) bool {
	return strings.Contains(content, "Streamy is live") || liveLinkPattern.MatchString(content)
}

func escapeMarkdown(value string) string {
	return markdownEscaper.Replace(value)
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// equalStrings compares secrets in constant time. Hashing first keeps the
// comparison length-independent, and the explicit length check preserves the
// Node implementation's semantics.
func equalStrings(a, b string) bool {
	left := sha256.Sum256([]byte(a))
	right := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1 && len(a) == len(b)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		headers.Set("Access-Control-Allow-Origin", "*")
		headers.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		headers.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

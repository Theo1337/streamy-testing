// Package config loads and stores Streamy's settings as JSON in the user's
// config directory.
//
// Two Go ideas are worth pointing out in this file:
//
//  1. We decode into a fully populated Default() value instead of into a zero
//     Config. encoding/json only overwrites the fields it finds, so settings
//     added in a later version automatically get their default value when an
//     older config.json is loaded. That is our migration story for free.
//
//  2. Saving is atomic: write a temporary file, then rename it over the real
//     one. A crash halfway through a write can therefore never leave you with a
//     half written config.json.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"streamy/internal/hotkey"
)

// CurrentSchema is stored in the file so a future version can recognise configs
// written by an older one.
const CurrentSchema = 3

// AppDirName is the folder created inside the operating system's config
// directory. On Windows that is %AppData%\streamy.
const AppDirName = "streamy"

const (
	DefaultSignalingURL = "wss://streamy-signal.thenightserver.workers.dev"
	DefaultViewerURL    = "https://streamy-viewer.pages.dev/"
)

// legacyViewerHost is the branch alias the viewer used before production
// deployments moved to the project's stable pages.dev domain. Cloudflare pins
// a branch alias to the first preview deployment on that branch, so the alias
// keeps serving an old build and must not be handed out any more.
const legacyViewerHost = "master.streamy-viewer.pages.dev"

// The values accepted by Capture.DefaultTarget.
const (
	TargetPrimary = "primary" // the primary monitor
	TargetFocused = "focused" // whatever window currently has focus
	TargetMonitor = "monitor" // the monitor at Capture.MonitorIndex
)

// The values accepted by Encoding.Encoder.
const (
	EncoderAuto  = "auto" // detect the best available encoder
	EncoderNVENC = "nvenc"
	EncoderQSV   = "qsv"
	EncoderAMF   = "amf"
	EncoderX264  = "x264" // software fallback
)

// Config is the whole settings tree. Every field has a json tag so the file is
// readable and hand-editable.
type Config struct {
	Schema int `json:"schema"`

	LogLevel      string `json:"logLevel"`
	SetupComplete bool   `json:"setupComplete"`
	RunAtStartup  bool   `json:"runAtStartup"`

	Hotkey    Hotkey    `json:"hotkey"`
	Capture   Capture   `json:"capture"`
	Indicator Indicator `json:"indicator"`
	Encoding  Encoding  `json:"encoding"`
	Stream    Stream    `json:"stream"`
	Signaling Signaling `json:"signaling"`
	Update    Update    `json:"update"`
}

// Hotkey holds the global shortcuts. Both are parsed by internal/hotkey, e.g.
// "Ctrl+Shift+F9".
type Hotkey struct {
	Enabled bool `json:"enabled"`
	// Toggle starts a stream when idle and stops it when streaming.
	Toggle string `json:"toggle"`
	// Picker always opens the screen/window picker (phase 6).
	Picker string `json:"picker"`
}

// The values accepted by Indicator.Position.
const (
	IndicatorTopLeft     = "top-left"
	IndicatorTopRight    = "top-right"
	IndicatorBottomLeft  = "bottom-left"
	IndicatorBottomRight = "bottom-right"
)

// Indicator configures the small blinking red dot shown on the streamer's own
// screen while a stream is live. It is always on top and stays out of the
// capture, so viewers never see it.
type Indicator struct {
	Enabled         bool `json:"enabled"`
	// Position is one of the Indicator* constants above.
	Position        string `json:"position"`
	// ShowViewerCount displays the number of viewers next to the live dot.
	ShowViewerCount bool `json:"showViewerCount"`
}

// Capture holds the "what do we record" settings, including the automatic
// target selection rules described in INSTRUCTIONS.md.
type Capture struct {
	// DefaultTarget is used when the hotkey cannot find a fullscreen app.
	DefaultTarget string `json:"defaultTarget"`
	// MonitorIndex is the monitor used when DefaultTarget is "monitor".
	MonitorIndex int `json:"monitorIndex"`
	// AlwaysShowPicker disables auto-selection entirely.
	AlwaysShowPicker bool `json:"alwaysShowPicker"`
	// DesktopAudio captures system render audio, including all apps
	// routed to that device. When ExcludeDiscordAudio is enabled, Windows
	// process loopback excludes the Discord desktop client's process tree.
	DesktopAudio        bool `json:"desktopAudio"`
	ExcludeDiscordAudio bool `json:"excludeDiscordAudio"`
}

// Encoding holds the ffmpeg related settings.
type Encoding struct {
	Encoder string `json:"encoder"`
	// FFmpegPath is an explicit path to ffmpeg.exe; empty means "look it up on
	// PATH".
	FFmpegPath string `json:"ffmpegPath"`
	// Resolution is "source" (no scaling) or an explicit "1920x1080".
	Resolution string `json:"resolution"`
	FPS        int    `json:"fps"`
	// BitrateKbps is the target video bitrate in kilobits per second.
	BitrateKbps int `json:"bitrateKbps"`
	// KeyframeSeconds is the GOP length. About one per second keeps new viewers
	// waiting at most a second before they can decode.
	KeyframeSeconds int `json:"keyframeSeconds"`
	// EncoderThreads is passed to libx264 (0 means "let ffmpeg decide").
	EncoderThreads int `json:"encoderThreads"`
	// ExtraEncoderArgs are appended to the ffmpeg command line, for tuning.
	ExtraEncoderArgs []string `json:"extraEncoderArgs"`
}

// Stream holds the WebRTC settings, the optional Discord Activity relay and
// the optional Discord bot notification.
type Stream struct {
	MaxViewers  int          `json:"maxViewers"`
	STUNServers []string     `json:"stunServers"`
	TURNServers []TURNServer `json:"turnServers"`
	// Discord configures the local bot that posts the public viewer link.
	Discord DiscordBot `json:"discord"`
	// Relay is the optional Discord Activity WebSocket output. It can run
	// alongside WebRTC or replace it entirely.
	Relay Relay `json:"relay"`
	// Stack controls the local bot/relay/gateway/tunnel process.
	Stack LocalBot `json:"stack"`
}

// Relay configures the publisher side of the Activity relay.
type Relay struct {
	// Enabled turns the WebSocket output on.
	Enabled bool `json:"enabled"`
	// URL is the relay base URL, e.g. wss://relay.example.com.
	URL string `json:"url"`
	// Token is the relay's deployment-wide publisher secret.
	Token string `json:"token"`
	// ChannelID is the fallback Discord channel whose Activity should share
	// this stream. When the streamer is in a voice channel, the bot resolves
	// that voice channel instead. The relay room ID is exactly the resolved
	// channel ID, so viewers in that channel's Activity join the same room.
	ChannelID string `json:"channelID"`
	// ActivityRoom is a fixed Discord channel ID to use as the Activity relay
	// room. When set, the desktop app publishes to this room directly instead
	// of resolving the streamer's voice channel. This allows the Activity to
	// work from any channel while all viewers join the same shared room.
	// If empty, the voice channel resolution is used (falling back to ChannelID).
	ActivityRoom string `json:"activityRoom"`
	// Only skips Cloudflare signaling and WebRTC entirely. Useful for a
	// Discord-only deployment.
	Only bool `json:"only"`
	// Cloudflared enables automatic Cloudflare tunnel (cloudflared) for the
	// Activity stack. When true, the Activity stack starts cloudflared and
	// registers the tunnel URL with the bot so the viewer link is public HTTPS.
	Cloudflared bool `json:"cloudflared"`
	// NotifyURL and NotifyKey let the bot post a Watch message when a stream
	// starts. Leave NotifyURL empty to disable notifications.
	NotifyURL string `json:"notifyURL"`
	NotifyKey string `json:"notifyKey"`
	// DiscordClientID is the Discord Application (Client) ID for the Activity OAuth flow.
	// If empty, it will be read from the viewer's config.json.
	DiscordClientID string `json:"discordClientID"`
	// DiscordClientSecret is the Discord Application secret for the Activity OAuth flow.
	// This must be obtained from the Discord Developer Portal.
	DiscordClientSecret string `json:"discordClientSecret"`
	// IdentitySecret is the HMAC secret for signing identity tokens (min 32 chars).
	// If empty, a random secret will be generated and saved on first run.
	IdentitySecret string `json:"identitySecret"`
}

// DiscordBot configures the local bot notification.
type DiscordBot struct {
	// ChannelID is the Discord channel that receives the public viewer link.
	ChannelID string `json:"channelID"`
	// NotifyURL and NotifyKey let the bot post or delete the link. Leave
	// NotifyURL empty to disable notifications.
	NotifyURL string `json:"notifyURL"`
	NotifyKey string `json:"notifyKey"`
}

// LocalBot configures the local Discord bot/Activity stack launcher.
//
// The desktop app runs the built-in `streamy bot` subcommand when Script is
// empty, and stops it again on quit. When the Activity relay is enabled, Script
// can point at start-activity.ps1 (development) or start-stack.ps1 (installed),
// which starts the relay, bot, gateway and Cloudflare tunnel together.
type LocalBot struct {
	// AutoStart launches the bot/stack together with Streamy.
	AutoStart bool `json:"autoStart"`
	// Script is an explicit path to a launcher script. Empty means use the
	// built-in Go bot, or auto-detect an Activity stack script.
	Script string `json:"script"`
	// PublishToken is passed to the relay as its publisher secret.
	PublishToken string `json:"publishToken"`
}

// UnmarshalJSON reads the Activity relay block that older configs stored under
// stream.relay. The same object is still valid today, but it used to be the
// only notification configuration, so a config with stream.discord empty also
// inherits its notify fields.
func (s *Stream) UnmarshalJSON(data []byte) error {
	type streamJSON Stream
	var raw struct {
		*streamJSON
		LegacyRelay *Relay `json:"relay"`
	}
	raw.streamJSON = (*streamJSON)(s)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// The explicit LegacyRelay field above shadows the embedded Relay field for
	// the "relay" key, so json.Unmarshal fills raw.LegacyRelay and leaves
	// s.Relay at its zero value. Copy it back: stream.relay is still the live
	// Activity configuration, not just a source of legacy notify settings.
	if raw.LegacyRelay != nil {
		s.Relay = *raw.LegacyRelay
		if s.Discord == (DiscordBot{}) {
			s.Discord = DiscordBot{
				ChannelID: raw.LegacyRelay.ChannelID,
				NotifyURL: raw.LegacyRelay.NotifyURL,
				NotifyKey: raw.LegacyRelay.NotifyKey,
			}
		}
	}
	return nil
}

// TURNServer is one relay, used when a direct peer-to-peer path is impossible.
type TURNServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// Signaling points at the Cloudflare Worker written in phase 4.
type Signaling struct {
	URL       string `json:"url"`
	ViewerURL string `json:"viewerURL"`
}

// Update controls automatic update checking from GitHub releases.
type Update struct {
	// Enabled turns on periodic update checks.
	Enabled bool `json:"enabled"`
	// Repo is the GitHub repository in "owner/repo" format.
	Repo string `json:"repo"`
	// CheckIntervalDays is how often to check for updates (0 = default 7 days).
	CheckIntervalDays int `json:"checkIntervalDays"`
	// AutoUpdateDisabled disables automatic periodic update checks.
	// If an update is marked as required, it will still notify regardless.
	AutoUpdateDisabled bool `json:"autoUpdateDisabled"`
}

// Default returns the settings used on first run, and the base that a loaded
// file is decoded on top of.
func Default() Config {
	return Config{
		Schema:       CurrentSchema,
		LogLevel:     "info",
		RunAtStartup: true,
		Hotkey: Hotkey{
			Enabled: true,
			Toggle:  "Ctrl+Shift+F9",
			Picker:  "Ctrl+Shift+F10",
		},
		Capture: Capture{
			DefaultTarget:       TargetPrimary,
			MonitorIndex:        0,
			DesktopAudio:        true,
			ExcludeDiscordAudio: true,
		},
		Indicator: Indicator{
			Enabled:          true,
			Position:         IndicatorTopRight,
			ShowViewerCount:  true,
		},
		Encoding: Encoding{
			Encoder:         EncoderAuto,
			Resolution:      "source",
			FPS:             30,
			BitrateKbps:     6000,
			KeyframeSeconds: 1,
		},
		Stream: Stream{
			MaxViewers: 10,
			// Google's public STUN servers: free, no account, good enough to
			// discover your public address for most home networks.
			STUNServers: []string{
				"stun:stun.l.google.com:19302",
				"stun:stun1.l.google.com:19302",
			},
			Relay: Relay{
				Enabled:    true,
				Only:       true,
				URL:        "ws://127.0.0.1:8080/.proxy/relay",
				Token:      "dev-token",
				ActivityRoom: "123456789012345678",
				Cloudflared: true,
			},
			Stack: LocalBot{AutoStart: true},
		},
		Signaling: Signaling{
			URL:       DefaultSignalingURL,
			ViewerURL: DefaultViewerURL,
		},
		Update: Update{
			Enabled:            true,
			Repo:               "Theo1337/streamy-app",
			CheckIntervalDays:  1,
			AutoUpdateDisabled: false,
		},
	}
}

// Dir returns the directory Streamy keeps its config and logs in.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(base, AppDirName), nil
}

// DefaultPath returns the path of the default config file.
func DefaultPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// LogPath returns the path of the default log file.
func LogPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "streamy.log"), nil
}

// Load reads the config from the default location.
func Load() (Config, bool, error) {
	path, err := DefaultPath()
	if err != nil {
		return Config{}, false, err
	}
	return LoadFrom(path)
}

// LoadFrom reads the config from path. A missing file is not an error: the
// defaults are written there and returned, with created reporting that this
// happened.
func LoadFrom(path string) (cfg Config, created bool, err error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = Default()
		if bundled, ok := bundledDefault(); ok {
			cfg = bundled
		}
		if err := cfg.SaveTo(path); err != nil {
			return cfg, false, err
		}
		return cfg, true, nil
	case err != nil:
		return Config{}, false, fmt.Errorf("read config: %w", err)
	}

	// The file is meant to be hand editable, and a good few Windows tools still
	// write a UTF-8 byte order mark at the start: PowerShell's Set-Content, and
	// any editor set to "UTF-8 with BOM". JSON does not allow one, and the
	// failure is nasty, because the settings look fine and are silently
	// ignored in favour of the defaults. Strip it instead of complaining.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	// Unknown fields are deliberately ignored, so a config written by a newer
	// version still loads in an older one.
	cfg = Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("parse config: %w", err)
	}
	if strings.TrimSpace(cfg.Signaling.URL) == "" {
		cfg.Signaling.URL = DefaultSignalingURL
	}
	if strings.TrimSpace(cfg.Signaling.ViewerURL) == "" {
		cfg.Signaling.ViewerURL = DefaultViewerURL
	}
	// Configs written before the branch change still point at the frozen
	// alias; rewrite them so shared links reach the current deployment.
	if isLegacyViewerURL(cfg.Signaling.ViewerURL) {
		cfg.Signaling.ViewerURL = DefaultViewerURL
	}
	if err := cfg.Validate(); err != nil {
		return cfg, false, err
	}
	return cfg, false, nil
}

// bundledDefault reads config.default.json next to the executable. An installer
// ships the user's Discord bot and stream settings there, so a fresh install has
// working notifications instead of the generic defaults. A malformed file is
// ignored: a bad packaging step must not stop Streamy from starting.
func bundledDefault() (Config, bool) {
	self, err := os.Executable()
	if err != nil {
		return Config{}, false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(self), "config.default.json"))
	if err != nil {
		return Config{}, false
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	cfg := Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, false
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, false
	}
	return cfg, true
}

// isLegacyViewerURL reports whether raw points at the branch alias that no
// longer receives new deployments.
func isLegacyViewerURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, legacyViewerHost)
}

// Save writes the config to the default location.
func (c Config) Save() error {
	path, err := DefaultPath()
	if err != nil {
		return err
	}
	return c.SaveTo(path)
}

// SaveTo writes the config to path atomically.
func (c Config) SaveTo(path string) error {
	c.Schema = CurrentSchema

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	// Write to a sibling temp file first so a partial write can never be
	// swapped in. os.Rename replaces the destination on both Windows and Unix.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort cleanup if we bail out before the rename.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Validate checks the values a user could have typed by hand, so mistakes are
// reported at startup instead of later inside ffmpeg.
func (c Config) Validate() error {
	var errs []error

	if c.Schema > CurrentSchema {
		errs = append(errs, fmt.Errorf("config schema %d is newer than this build understands (%d)", c.Schema, CurrentSchema))
	}

	// Validate activityRoom is a valid Discord channel ID when set
	if id := strings.TrimSpace(c.Stream.Relay.ActivityRoom); id != "" && !discordChannelRe.MatchString(id) {
		errs = append(errs, errors.New("stream.relay.activityRoom: must be a Discord channel ID"))
	}

	if c.Hotkey.Enabled {
		if _, err := hotkey.Parse(c.Hotkey.Toggle); err != nil {
			errs = append(errs, fmt.Errorf("hotkey.toggle: %w", err))
		}
	}
	if _, err := hotkey.Parse(c.Hotkey.Picker); err != nil {
		errs = append(errs, fmt.Errorf("hotkey.picker: %w", err))
	}

	switch c.Capture.DefaultTarget {
	case TargetPrimary, TargetFocused, TargetMonitor:
	case "":
		errs = append(errs, errors.New("capture.defaultTarget: must not be empty"))
	default:
		errs = append(errs, fmt.Errorf("capture.defaultTarget: unknown value %q", c.Capture.DefaultTarget))
	}
	if c.Capture.MonitorIndex < 0 {
		errs = append(errs, errors.New("capture.monitorIndex: must not be negative"))
	}

	switch c.Indicator.Position {
	case IndicatorTopLeft, IndicatorTopRight, IndicatorBottomLeft, IndicatorBottomRight:
	case "":
		errs = append(errs, errors.New("indicator.position: must not be empty"))
	default:
		errs = append(errs, fmt.Errorf("indicator.position: unknown value %q (want top-left, top-right, bottom-left or bottom-right)", c.Indicator.Position))
	}

	switch c.Encoding.Encoder {
	case EncoderAuto, EncoderNVENC, EncoderQSV, EncoderAMF, EncoderX264:
	case "":
		errs = append(errs, errors.New("encoding.encoder: must not be empty"))
	default:
		errs = append(errs, fmt.Errorf("encoding.encoder: unknown value %q (want auto, nvenc, qsv, amf or x264)", c.Encoding.Encoder))
	}
	if err := validateResolution(c.Encoding.Resolution); err != nil {
		errs = append(errs, err)
	}
	if c.Encoding.FPS < 1 || c.Encoding.FPS > 240 {
		errs = append(errs, fmt.Errorf("encoding.fps: %d is out of range (1-240)", c.Encoding.FPS))
	}
	if c.Encoding.BitrateKbps < 100 {
		errs = append(errs, fmt.Errorf("encoding.bitrateKbps: %d is too low (minimum 100)", c.Encoding.BitrateKbps))
	}
	if c.Encoding.KeyframeSeconds < 1 || c.Encoding.KeyframeSeconds > 10 {
		errs = append(errs, fmt.Errorf("encoding.keyframeSeconds: %d is out of range (1-10)", c.Encoding.KeyframeSeconds))
	}
	if c.Encoding.EncoderThreads < 0 {
		errs = append(errs, errors.New("encoding.encoderThreads: must not be negative"))
	}

	if c.Stream.MaxViewers < 0 {
		errs = append(errs, errors.New("stream.maxViewers: must not be negative"))
	} else if c.Stream.MaxViewers > 20 {
		errs = append(errs, errors.New("stream.maxViewers: must not exceed the Worker hard limit of 20"))
	}
	for i, server := range c.Stream.STUNServers {
		if err := validateICEServerURL(server, "stun", "stuns"); err != nil {
			errs = append(errs, fmt.Errorf("stream.stunServers[%d]: %w", i, err))
		}
	}
	for i, server := range c.Stream.TURNServers {
		if len(server.URLs) == 0 {
			errs = append(errs, fmt.Errorf("stream.turnServers[%d].urls: must contain at least one URL", i))
		}
		for j, serverURL := range server.URLs {
			if err := validateICEServerURL(serverURL, "turn", "turns"); err != nil {
				errs = append(errs, fmt.Errorf("stream.turnServers[%d].urls[%d]: %w", i, j, err))
			}
		}
	}

	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Errorf("logLevel: unknown level %q", c.LogLevel))
	}

	if u := strings.TrimSpace(c.Signaling.URL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil {
			errs = append(errs, fmt.Errorf("signaling.url: %w", err))
		} else if (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" {
			errs = append(errs, fmt.Errorf("signaling.url: must be an absolute ws or wss URL, got %q", u))
		}
	}
	if u := strings.TrimSpace(c.Signaling.ViewerURL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil {
			errs = append(errs, fmt.Errorf("signaling.viewerURL: %w", err))
		} else if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			errs = append(errs, errors.New("signaling.viewerURL: must be an absolute http or https URL"))
		}
	}

	if c.Update.Enabled {
		if strings.TrimSpace(c.Update.Repo) == "" {
			errs = append(errs, errors.New("update.repo: must not be empty when update is enabled"))
		} else if !strings.Contains(c.Update.Repo, "/") {
			errs = append(errs, errors.New("update.repo: must be in 'owner/repo' format"))
		}
		if c.Update.CheckIntervalDays < 0 {
			errs = append(errs, errors.New("update.checkIntervalDays: must not be negative"))
		}
	}

	discord := c.Stream.Discord
	if u := strings.TrimSpace(discord.NotifyURL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil {
			errs = append(errs, fmt.Errorf("stream.discord.notifyURL: %w", err))
		} else if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			errs = append(errs, errors.New("stream.discord.notifyURL: must be an absolute http or https URL"))
		}
		if strings.TrimSpace(discord.NotifyKey) == "" {
			errs = append(errs, errors.New("stream.discord.notifyKey: required when notifyURL is set"))
		}
	}
	if id := strings.TrimSpace(discord.ChannelID); id != "" && !discordChannelRe.MatchString(id) {
		errs = append(errs, errors.New("stream.discord.channelID: must be a Discord channel ID"))
	}

	relay := c.Stream.Relay
	if relay.Only && !relay.Enabled {
		errs = append(errs, errors.New("stream.relay.only: requires stream.relay.enabled"))
	}
	// activityRoom validation moved to the main validation section above
	if relay.Enabled {
		if u := strings.TrimSpace(relay.URL); u == "" {
			errs = append(errs, errors.New("stream.relay.url: required when the relay is enabled"))
		} else if parsed, err := url.Parse(u); err != nil {
			errs = append(errs, fmt.Errorf("stream.relay.url: %w", err))
		} else if (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" {
			errs = append(errs, errors.New("stream.relay.url: must be an absolute ws or wss URL"))
		}
		if strings.TrimSpace(relay.Token) == "" {
			errs = append(errs, errors.New("stream.relay.token: required when the relay is enabled"))
		}
	}
	if id := strings.TrimSpace(relay.ChannelID); id != "" && !discordChannelRe.MatchString(id) {
		errs = append(errs, errors.New("stream.relay.channelID: must be a Discord channel ID"))
	}
	if u := strings.TrimSpace(relay.NotifyURL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil {
			errs = append(errs, fmt.Errorf("stream.relay.notifyURL: %w", err))
		} else if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			errs = append(errs, errors.New("stream.relay.notifyURL: must be an absolute http or https URL"))
		}
		if strings.TrimSpace(relay.NotifyKey) == "" {
			errs = append(errs, errors.New("stream.relay.notifyKey: required when notifyURL is set"))
		}
	}

	return errors.Join(errs...)
}

func validateICEServerURL(raw string, schemes ...string) error {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Opaque == "" && parsed.Host == "" {
		return errors.New("must include a server address")
	}
	for _, scheme := range schemes {
		if parsed.Scheme == scheme {
			return nil
		}
	}
	return fmt.Errorf("scheme must be %s", strings.Join(schemes, " or "))
}

// discordChannelRe matches a Discord snowflake as accepted by the bot.
var discordChannelRe = regexp.MustCompile(`^\d{5,32}$`)

// resolutionRe matches "1920x1080" and similar.
var resolutionRe = regexp.MustCompile(`^(\d{2,5})x(\d{2,5})$`)

func validateResolution(res string) error {
	value := strings.ToLower(strings.TrimSpace(res))
	if value == "source" {
		return nil
	}
	m := resolutionRe.FindStringSubmatch(value)
	if m == nil {
		return fmt.Errorf("encoding.resolution: %q is not \"source\" or WIDTHxHEIGHT", res)
	}
	for _, part := range m[1:] {
		n, err := strconv.Atoi(part)
		if err != nil || n == 0 {
			return fmt.Errorf("encoding.resolution: %q has a non positive dimension", res)
		}
	}
	return nil
}

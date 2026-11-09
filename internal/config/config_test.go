package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}
	if Default().Signaling.URL != DefaultSignalingURL || Default().Signaling.ViewerURL != DefaultViewerURL {
		t.Fatalf("default endpoints = %+v, want bundled deployment endpoints", Default().Signaling)
	}
	if !Default().Capture.DesktopAudio {
		t.Fatal("desktop audio should be enabled by default")
	}
	if !Default().Capture.ExcludeDiscordAudio {
		t.Fatal("Discord audio should be excluded by default")
	}
	if !Default().Indicator.Enabled || Default().Indicator.Position != IndicatorTopRight {
		t.Fatalf("indicator defaults = %+v, want an enabled top right dot", Default().Indicator)
	}
}

func TestLoadFromWritesDefaultsWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg, created, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !created {
		t.Error("created = false, want true for a missing file")
	}
	if got, want := cfg.Hotkey.Toggle, Default().Hotkey.Toggle; got != want {
		t.Errorf("hotkey.toggle = %q, want %q", got, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file was not written: %v", err)
	}
}

// A config written by an older version is missing the fields added since. They
// must come back as their default value, not as the zero value.
func TestLoadFromToleratesAByteOrderMark(t *testing.T) {
	// PowerShell's Set-Content and any editor set to "UTF-8 with BOM" produce
	// this. The settings inside are valid, and silently falling back to the
	// defaults would look like the file was being ignored for no reason.
	for _, tc := range []struct{ name, prefix string }{
		{"UTF-8 BOM", "\xef\xbb\xbf"},
		{"no BOM", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			body := `{"logLevel":"debug","encoding":{"fps":24},"capture":{"defaultTarget":"primary"}}`
			if err := os.WriteFile(path, []byte(tc.prefix+body), 0o644); err != nil {
				t.Fatal(err)
			}

			cfg, _, err := LoadFrom(path)
			if err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			if cfg.LogLevel != "debug" || cfg.Encoding.FPS != 24 {
				t.Errorf("logLevel = %q, fps = %d; want \"debug\" and 24 from the file",
					cfg.LogLevel, cfg.Encoding.FPS)
			}
			if cfg.Capture.DefaultTarget != TargetPrimary {
				t.Errorf("capture.defaultTarget = %q, want %q", cfg.Capture.DefaultTarget, TargetPrimary)
			}
			if !cfg.Capture.DesktopAudio {
				t.Error("missing capture.desktopAudio should use the enabled default")
			}
			if !cfg.Capture.ExcludeDiscordAudio {
				t.Error("missing capture.excludeDiscordAudio should use the enabled default")
			}
			if cfg.SetupComplete || !cfg.RunAtStartup {
				t.Errorf("missing setup/startup settings = %t/%t, want false/true", cfg.SetupComplete, cfg.RunAtStartup)
			}
			if cfg.Signaling.URL != DefaultSignalingURL || cfg.Signaling.ViewerURL != DefaultViewerURL {
				t.Errorf("missing service endpoints = %+v, want bundled defaults", cfg.Signaling)
			}
			// A BOM must not be mistaken for the start of the file's content,
			// so the first key must still be parsed, not dropped.
			if cfg.Encoding.BitrateKbps != Default().Encoding.BitrateKbps {
				t.Errorf("encoding.bitrateKbps = %d, want the default", cfg.Encoding.BitrateKbps)
			}
		})
	}
}

func TestLoadFromFillsEmptyBundledServiceEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"signaling":{"url":"","viewerURL":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Signaling.URL != DefaultSignalingURL || cfg.Signaling.ViewerURL != DefaultViewerURL {
		t.Fatalf("signaling = %+v, want bundled defaults", cfg.Signaling)
	}
}

func TestLoadFromMovesLegacyViewerAliasToProduction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{"signaling":{"viewerURL":"https://master.streamy-viewer.pages.dev/"}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Signaling.ViewerURL != DefaultViewerURL {
		t.Fatalf("viewerURL = %q, want %q", cfg.Signaling.ViewerURL, DefaultViewerURL)
	}
}

func TestSavedConfigHasNoByteOrderMark(t *testing.T) {
	// The app must not write what it has just learned to tolerate: a BOM of our
	// own would make the file unreadable to stricter tools.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Default().SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		t.Error("the saved config starts with a UTF-8 byte order mark")
	}
}

func TestLoadFromKeepsDefaultsForMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// Only logLevel and fps are present, and fps is intentionally non default.
	partial := `{"logLevel":"debug","encoding":{"fps":60}}`
	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, created, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if created {
		t.Error("created = true, want false for an existing file")
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("logLevel = %q, want \"debug\"", cfg.LogLevel)
	}
	if cfg.Encoding.FPS != 60 {
		t.Errorf("encoding.fps = %d, want 60 (the value from the file)", cfg.Encoding.FPS)
	}
	// Not mentioned in the file at all.
	if cfg.Encoding.BitrateKbps != Default().Encoding.BitrateKbps {
		t.Errorf("encoding.bitrateKbps = %d, want the default %d",
			cfg.Encoding.BitrateKbps, Default().Encoding.BitrateKbps)
	}
	if len(cfg.Stream.STUNServers) == 0 {
		t.Error("stream.stunServers is empty, want the defaults")
	}
}

func TestLoadFromMigratesLegacyRelayNotification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{
		"schema": 2,
		"stream": {
			"relay": {
				"enabled": true,
				"url": "wss://relay.example.com",
				"token": "publisher-secret",
				"channelID": "123456789012345678",
				"only": true,
				"notifyURL": "http://127.0.0.1:8791/api/notify",
				"notifyKey": "notify-secret"
			}
		}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, created, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if created {
		t.Fatal("LoadFrom reported a created file for an existing config")
	}
	wantDiscord := DiscordBot{
		ChannelID: "123456789012345678",
		NotifyURL: "http://127.0.0.1:8791/api/notify",
		NotifyKey: "notify-secret",
	}
	if cfg.Stream.Discord != wantDiscord {
		t.Fatalf("stream.discord = %+v, want %+v", cfg.Stream.Discord, wantDiscord)
	}
	// stream.relay is still the live Activity configuration. The custom
	// Stream.UnmarshalJSON must not consume it only to migrate notifications.
	wantRelay := Relay{
		Enabled:   true,
		URL:       "wss://relay.example.com",
		Token:     "publisher-secret",
		ChannelID: "123456789012345678",
		Only:      true,
		NotifyURL: "http://127.0.0.1:8791/api/notify",
		NotifyKey: "notify-secret",
	}
	if cfg.Stream.Relay != wantRelay {
		t.Fatalf("stream.relay = %+v, want %+v", cfg.Stream.Relay, wantRelay)
	}
}

func TestSaveToRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	want := Default()
	want.SetupComplete = true
	want.RunAtStartup = true
	want.Hotkey.Toggle = "Alt+Win+P"
	want.Stream.MaxViewers = 3
	if err := want.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	got, _, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got.Hotkey.Toggle != want.Hotkey.Toggle {
		t.Errorf("hotkey.toggle = %q, want %q", got.Hotkey.Toggle, want.Hotkey.Toggle)
	}
	if got.Stream.MaxViewers != want.Stream.MaxViewers {
		t.Errorf("stream.maxViewers = %d, want %d", got.Stream.MaxViewers, want.Stream.MaxViewers)
	}
	if got.SetupComplete != want.SetupComplete || got.RunAtStartup != want.RunAtStartup {
		t.Errorf("setup/startup = %t/%t, want %t/%t", got.SetupComplete, got.RunAtStartup, want.SetupComplete, want.RunAtStartup)
	}
}

func TestSaveToIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if err := Default().SaveTo(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want only config.json", names)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bare key hotkey", func(c *Config) { c.Hotkey.Toggle = "F9" }},
		{"unknown key", func(c *Config) { c.Hotkey.Toggle = "Ctrl+Nothing" }},
		{"unknown encoder", func(c *Config) { c.Encoding.Encoder = "gpu-go-brrr" }},
		{"bad resolution", func(c *Config) { c.Encoding.Resolution = "1080" }},
		{"zero fps", func(c *Config) { c.Encoding.FPS = 0 }},
		{"silly fps", func(c *Config) { c.Encoding.FPS = 1000 }},
		{"bitrate too low", func(c *Config) { c.Encoding.BitrateKbps = 5 }},
		{"negative viewers", func(c *Config) { c.Stream.MaxViewers = -1 }},
		{"viewers above Worker limit", func(c *Config) { c.Stream.MaxViewers = 21 }},
		{"invalid STUN URL", func(c *Config) { c.Stream.STUNServers = []string{"turn:relay.example"} }},
		{"invalid TURN URL", func(c *Config) { c.Stream.TURNServers = []TURNServer{{URLs: []string{"stun:stun.example"}}} }},
		{"bad default target", func(c *Config) { c.Capture.DefaultTarget = "everything" }},
		{"bad indicator position", func(c *Config) { c.Indicator.Position = "middle" }},
		{"empty indicator position", func(c *Config) { c.Indicator.Position = "" }},
		{"bad log level", func(c *Config) { c.LogLevel = "verbose" }},
		{"signaling without scheme", func(c *Config) { c.Signaling.URL = "example.com" }},
		{"signaling with http", func(c *Config) { c.Signaling.URL = "https://example.com" }},
		{"signaling without host", func(c *Config) { c.Signaling.URL = "wss:" }},
		{"viewer signaling with ws", func(c *Config) { c.Signaling.ViewerURL = "wss://example.com" }},
		{"notify without key", func(c *Config) {
			c.Stream.Discord = DiscordBot{NotifyURL: "https://bot.example.com/api/notify"}
		}},
		{"notify with invalid URL", func(c *Config) {
			c.Stream.Discord = DiscordBot{NotifyURL: "not-a-url", NotifyKey: "k"}
		}},
		{"invalid channel ID", func(c *Config) { c.Stream.Discord.ChannelID = "general" }},
		{"future schema", func(c *Config) { c.Schema = CurrentSchema + 1 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
		})
	}
}

func TestValidateAcceptsGoodValues(t *testing.T) {
	cfg := Default()
	cfg.Hotkey.Toggle = "Win+Alt+S"
	cfg.Encoding.Resolution = "1920x1080"
	cfg.Encoding.Encoder = EncoderNVENC
	cfg.Signaling.URL = "wss://streamy-signal.example.workers.dev"
	cfg.Signaling.ViewerURL = "https://streamy-viewer.pages.dev/"
	cfg.Stream.MaxViewers = 0 // unlimited
	cfg.Indicator.Position = IndicatorBottomLeft
	cfg.Stream.Discord = DiscordBot{
		ChannelID: "123456789012345678",
		NotifyURL: "https://bot.example.com/api/notify",
		NotifyKey: "notify-secret",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// The file is meant to be hand editable, so make sure it is indented and uses
// the short json names people see in the README.
func TestSavedJSONIsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Default().SaveTo(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("saved config is not valid json: %v", err)
	}
	if _, ok := raw["hotkey"]; !ok {
		t.Error("saved config has no \"hotkey\" key")
	}
	if string(data)[0] != '{' {
		t.Error("saved config does not start with a brace")
	}
	if !contains(data, '\n') {
		t.Error("saved config is a single line, want indented json")
	}
}

func contains(b []byte, c byte) bool {
	for _, x := range b {
		if x == c {
			return true
		}
	}
	return false
}

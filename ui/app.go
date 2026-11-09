package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"streamy/internal/config"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type SettingsApp struct {
	address string
	token   string
	view    string
	client  *http.Client
	ctx     context.Context
}

func NewSettingsApp(address, token, view string) *SettingsApp {
	return &SettingsApp{
		address: "http://" + address,
		token:   token,
		view:    view,
		client:  &http.Client{Timeout: 45 * time.Second},
	}
}

func (a *SettingsApp) startup(ctx context.Context) {
	a.ctx = ctx
	go setWindowIcon()
}
func (a *SettingsApp) shutdown(context.Context) {}

// GetView tells the frontend whether this window is the settings page or the
// full-window screen picker. The picker deliberately has no sidebar.
func (a *SettingsApp) GetView() string { return a.view }

func (a *SettingsApp) GetConfig() (config.Config, error) {
	var cfg config.Config
	err := a.request(http.MethodGet, "/api/config", nil, &cfg)
	return cfg, err
}

func (a *SettingsApp) SaveConfig(cfg config.Config) error {
	return a.request(http.MethodPut, "/api/config", cfg, nil)
}

func (a *SettingsApp) GetTargets() (map[string]any, error) {
	var targets map[string]any
	err := a.request(http.MethodGet, "/api/targets", nil, &targets)
	return targets, err
}

func (a *SettingsApp) GetPreview(kind string, index int, handle string) (string, error) {
	path := fmt.Sprintf("/api/preview?kind=%s&index=%d&handle=%s",
		url.QueryEscape(kind), index, url.QueryEscape(handle))
	req, err := http.NewRequestWithContext(a.ctx, http.MethodGet, a.address+path, nil)
	if err != nil {
		return "", fmt.Errorf("create preview request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	response, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request capture preview: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read capture preview: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("capture preview failed (%s): %s", response.Status, strings.TrimSpace(string(data)))
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (a *SettingsApp) StartShare(kind string, index int, handle string) error {
	return a.request(http.MethodPost, "/api/share", map[string]any{
		"kind": kind, "index": index, "handle": handle,
	}, nil)
}

// SwitchShare retargets a stream that is already live. The room and viewer
// link stay the same; only the captured window or display changes.
func (a *SettingsApp) SwitchShare(kind string, index int, handle string) error {
	return a.request(http.MethodPost, "/api/switch", map[string]any{
		"kind": kind, "index": index, "handle": handle,
	}, nil)
}

// SetRunAtStartup applies the Windows login setting immediately, so the toggle
// is not silently lost when the window is closed without saving the rest of the
// form.
func (a *SettingsApp) SetRunAtStartup(enabled bool) error {
	return a.request(http.MethodPost, "/api/startup", map[string]bool{"enabled": enabled}, nil)
}

func (a *SettingsApp) StopShare() error {
	return a.request(http.MethodPost, "/api/stop", struct{}{}, nil)
}

func (a *SettingsApp) GetStatus() (map[string]any, error) {
	var status map[string]any
	err := a.request(http.MethodGet, "/api/status", nil, &status)
	return status, err
}

func (a *SettingsApp) GetUpdateProgress() (map[string]any, error) {
	var progress map[string]any
	err := a.request(http.MethodGet, "/api/update", nil, &progress)
	return progress, err
}

func (a *SettingsApp) CheckForUpdates() (map[string]any, error) {
	var result map[string]any
	err := a.request(http.MethodPost, "/api/check-updates", struct{}{}, &result)
	return result, err
}

func (a *SettingsApp) InstallUpdate() error {
	return a.request(http.MethodPost, "/api/install-update", struct{}{}, nil)
}

func (a *SettingsApp) GetDiscordIdentity() (map[string]any, error) {
	var identity map[string]any
	err := a.request(http.MethodGet, "/api/discord/profile", nil, &identity)
	return identity, err
}

func (a *SettingsApp) StartDiscordLogin() error {
	var result struct {
		AuthorizationURL string `json:"authorizationURL"`
	}
	if err := a.request(http.MethodPost, "/api/discord/login", struct{}{}, &result); err != nil {
		return err
	}
	if result.AuthorizationURL == "" {
		return fmt.Errorf("Discord OAuth service returned an empty authorization URL")
	}
	runtime.BrowserOpenURL(a.ctx, result.AuthorizationURL)
	return nil
}

func (a *SettingsApp) DisconnectDiscord() error {
	return a.request(http.MethodDelete, "/api/discord/profile", nil, nil)
}

func (a *SettingsApp) CloseWindow() {
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

func (a *SettingsApp) request(method, path string, body, output any) error {
	var input *bytes.Reader
	if body == nil {
		input = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(a.ctx, method, a.address+path, input)
	if err != nil {
		return fmt.Errorf("create tray request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("contact Streamy tray: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		if readErr != nil {
			return fmt.Errorf("tray request failed (%s); read error response: %w", response.Status, readErr)
		}
		return fmt.Errorf("tray request failed (%s): %s", response.Status, strings.TrimSpace(string(message)))
	}
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			return fmt.Errorf("decode tray response: %w", err)
		}
	}
	return nil
}

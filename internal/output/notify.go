package output

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BotNotice is the payload the bot uses to post or remove the live viewer link.
// Action is "start" or "stop"; an empty action means start.
// For Discord Activity, "resolve" asks the bot to find the channel from the user's
// voice state and returns the channel ID.
type BotNotice struct {
	Action    string `json:"action,omitempty"`
	ChannelID string `json:"channelId"`
	Streamer  string `json:"streamer,omitempty"`
	URL       string `json:"url,omitempty"`
	UserID    string `json:"userId,omitempty"`
}

// NotifyBotStart tells the bot to post the public viewer link. The API key is a
// bearer token shared only by the desktop app and the bot.
func NotifyBotStart(ctx context.Context, endpoint, apiKey string, notice BotNotice) error {
	notice.Action = "start"
	_, err := callBot(ctx, endpoint, apiKey, notice)
	return err
}

// NotifyBotStop asks the bot to delete the live viewer link because there is no
// stream any more.
func NotifyBotStop(ctx context.Context, endpoint, apiKey string, notice BotNotice) error {
	notice.Action = "stop"
	_, err := callBot(ctx, endpoint, apiKey, notice)
	return err
}

// ResolveResponse is the response from the bot's resolve action.
type ResolveResponse struct {
	OK       bool   `json:"ok"`
	ChannelID string `json:"channelId"`
	Voice    bool   `json:"voice"`
}

// NotifyBotResolve asks the bot to find the target channel from the user's voice
// state. Returns the channel ID and whether it's a voice channel.
func NotifyBotResolve(ctx context.Context, endpoint, apiKey string, channelID, userID string) (ResolveResponse, error) {
	var zero ResolveResponse
	notice := BotNotice{
		Action:    "resolve",
		ChannelID: channelID,
		UserID:    userID,
	}
	body, err := callBot(ctx, endpoint, apiKey, notice)
	if err != nil {
		return zero, err
	}
	var resp ResolveResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return zero, fmt.Errorf("parse resolve response: %w", err)
	}
	return resp, nil
}

func callBot(ctx context.Context, endpoint, apiKey string, notice BotNotice) ([]byte, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, nil
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("bot notify URL is configured but the API key is empty")
	}
	body, err := json.Marshal(notice)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 8 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("bot request failed (HTTP %s): %s", response.Status, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}

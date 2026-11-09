package stack

import (
	"errors"
	"strings"
	"testing"

	"streamy/internal/config"
)

func TestFinishAllowsRestartAfterFailure(t *testing.T) {
	m := &Manager{started: true, generation: 3}

	m.finish(3, errors.New("exit status 1"), false)

	if m.started {
		t.Fatal("started is still true after a failed start; a later Start would be ignored")
	}
	if m.running {
		t.Fatal("running is true after a failed start")
	}
	if m.err == nil {
		t.Fatal("the startup error was not recorded")
	}
}

func TestFinishKeepsStartedForARunningChild(t *testing.T) {
	m := &Manager{started: true, generation: 2}

	m.finish(2, nil, true)

	if !m.started {
		t.Fatal("started was cleared while the child is running")
	}
	if !m.running {
		t.Fatal("running was not set")
	}
}

func TestFinishIgnoresAStaleGeneration(t *testing.T) {
	m := &Manager{started: true, generation: 4}

	m.finish(3, errors.New("exit status 1"), false)

	if !m.started {
		t.Fatal("a stale finish cleared the state of a newer generation")
	}
}

func TestPortFromNotifyURL(t *testing.T) {
	cases := map[string]int{
		"":                                   defaultBotPort,
		"not a url":                          defaultBotPort,
		"http://127.0.0.1/api/notify":        defaultBotPort,
		"http://127.0.0.1:9000/api/notify":   9000,
		"http://127.0.0.1:99999/api/notify":  defaultBotPort,
		"https://example.com:443/api/notify": 443,
	}
	for input, want := range cases {
		if got := portFromNotifyURL(input); got != want {
			t.Errorf("portFromNotifyURL(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestLocalActivityAddresses(t *testing.T) {
	type want struct {
		gateway string
		relay   string
		bot     string
		ok      bool
	}
	cases := map[string]want{
		// The common local layout: publish through the gateway proxy.
		"ws://127.0.0.1:8080/.proxy/relay": {":8080", ":8081", ":8083", true},
		// Direct relay: the URL port is the relay, so the gateway moves aside.
		"ws://127.0.0.1:8081": {":8080", ":8081", ":8083", true},
		"ws://localhost:9000/.proxy/relay": {
			gateway: ":9000", relay: ":8081", bot: ":8083", ok: true,
		},
		// Collisions still produce three distinct ports.
		"ws://127.0.0.1:8081/.proxy/relay": {":8081", ":18081", ":8083", true},
		"ws://127.0.0.1:8080":              {":18080", ":8080", ":8083", true},
		"ws://127.0.0.1:8083":              {":8080", ":8083", ":18083", true},
		// Remote or TLS relays are somebody else's deployment.
		"wss://relay.example.com":           {"", "", "", false},
		"ws://192.168.1.10:8080":            {"", "", "", false},
		"wss://127.0.0.1:8080/.proxy/relay": {"", "", "", false},
		"ws://127.0.0.1/.proxy/relay":       {"", "", "", false},
	}

	for input, expected := range cases {
		gateway, relay, bot, ok := localActivityAddresses(true, input)
		if ok != expected.ok || gateway != expected.gateway || relay != expected.relay || bot != expected.bot {
			t.Errorf("localActivityAddresses(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				input, gateway, relay, bot, ok,
				expected.gateway, expected.relay, expected.bot, expected.ok)
		}
	}

	if _, _, _, ok := localActivityAddresses(false, "ws://127.0.0.1:8080/.proxy/relay"); ok {
		t.Error("a disabled relay must not start the Activity stack")
	}
}

func TestNewSelectsActivityModeForLocalRelay(t *testing.T) {
	m := New(nil, Options{
		Relay: config.Relay{
			Enabled: true,
			URL:     "ws://127.0.0.1:8080/.proxy/relay",
			Token:   "publisher-secret",
		},
		LogDir: t.TempDir(),
	})
	if m.mode != "activity" {
		t.Fatalf("mode = %q, want activity", m.mode)
	}
	if m.healthAddr != "127.0.0.1:8080" {
		t.Fatalf("healthAddr = %q, want 127.0.0.1:8080", m.healthAddr)
	}
	if m.publishToken != "publisher-secret" {
		t.Fatalf("publishToken = %q, want the relay token", m.publishToken)
	}

	args := strings.Join(m.activityArgs(), " ")
	for _, want := range []string{
		"activity -gateway :8080 -relay :8081 -bot :8083",
		"-publish-token publisher-secret",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("activityArgs() = %q, missing %q", args, want)
		}
	}
}

func TestNewLeavesRemoteRelayAlone(t *testing.T) {
	m := New(nil, Options{
		Relay: config.Relay{
			Enabled: true,
			URL:     "wss://relay.example.com",
		},
	})
	if m.mode != "bot" {
		t.Fatalf("mode = %q, want bot for a remote relay", m.mode)
	}
}

func TestActivityArgsCarriesViewerLimitAndLogLevel(t *testing.T) {
	m := &Manager{
		mode:         "activity",
		gatewayAddr:  ":9000",
		relayAddr:    ":9001",
		botAddr:      ":9002",
		publishToken: "secret",
		maxViewers:   5,
		logLevel:     "debug",
	}
	got := strings.Join(m.activityArgs(), " ")
	want := "activity -gateway :9000 -relay :9001 -bot :9002 -publish-token secret -max-viewers 5 -log-level debug"
	if got != want {
		t.Fatalf("activityArgs() = %q, want %q", got, want)
	}
}

func TestHealthAddr(t *testing.T) {
	cases := map[string]string{
		":8080":          "127.0.0.1:8080",
		"0.0.0.0:8080":   "127.0.0.1:8080",
		"[::]:8080":      "127.0.0.1:8080",
		"127.0.0.1:80":   "127.0.0.1:80",
		"localhost:8080": "localhost:8080",
	}
	for input, want := range cases {
		if got := healthAddr(input); got != want {
			t.Errorf("healthAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

// Package stack owns the lifecycle of the local helper processes: the
// notification bot (`streamy bot`) and the Activity stack (`streamy activity`,
// which runs the relay, gateway and activity bot together).
//
// Both are subcommands of the same executable, so the tray starts a child
// process instead of a script. A configured script is still honoured for
// development and for people who point config.json at their own launcher; an
// empty script uses the built-in processes.
package stack

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"streamy/internal/config"
	"streamy/internal/executil"
)

const (
	defaultBotPort         = 8791
	defaultGatewayPort     = 8080
	defaultRelayPort       = 8081
	defaultActivityBotPort = 8083
	startupTimeout         = 30 * time.Second
	stopTimeout            = 10 * time.Second
)

// Options carries everything the manager needs to choose and launch the right
// child. Relay decides whether the built-in Activity stack should run: when it
// is enabled and points at this machine, the tray starts `streamy activity`
// instead of the notification bot.
type Options struct {
	Settings     config.LocalBot
	Relay        config.Relay
	NotifyURL    string
	NotifyKey    string
	SignalingURL string // Worker front door URL (e.g., wss://streamy-signal.thenightserver.workers.dev)
	LogDir       string
	LogLevel     string
	MaxViewers   int
	// Executable overrides the binary used for child processes. Empty means
	// the running executable, which is what ships; tests and embedders can
	// point it at a built streamy binary.
	Executable string
	// ViewerDir is the directory containing the built viewer static files.
	// If empty, the gateway uses its embedded viewer.
	ViewerDir string
}

// Manager is safe to call from several goroutines. Start is idempotent and
// Stop only stops a child that this process started (or adopted after a restart
// when the child supports a graceful quit).
type Manager struct {
	logger            *slog.Logger
	mode              string // "bot" or "activity"
	script            string
	publishToken      string
	port              int
	apiKey            string
	logDir            string
	logLevel          string
	maxViewers        int
	executable        string
	cloudflared       bool
	cloudflaredBin    string
	viewerDir         string
	discordClientID   string
	discordSecret     string
	identitySecret    string
	workerURL         string
	workerToken       string

	gatewayAddr string
	relayAddr   string
	botAddr     string
	healthAddr  string

	mu         sync.Mutex
	started    bool
	running    bool
	owned      bool
	generation int
	cancel     context.CancelFunc
	procDone   chan struct{}
	err        error
}

// New builds a manager. notifyURL supplies the port the notification bot must
// listen on; notifyKey is used to ask an already-running bot to quit gracefully.
// The Activity stack is selected when the relay is enabled and its URL points
// at a loopback address; a configured (or auto-detected) script always wins
// because it may start more than these processes.
func New(logger *slog.Logger, opts Options) *Manager {
	if logger == nil {
		logger = slog.Default()
	}

	settings := opts.Settings
	script := strings.TrimSpace(settings.Script)
	if script == "" {
		script = defaultActivityScript()
	}

	gateway, relay, bot, localActivity := localActivityAddresses(opts.Relay.Enabled, opts.Relay.URL)
	// The built-in `streamy activity` command owns the local stack. A legacy
	// PowerShell launcher (start-activity.ps1 / start-stack.ps1) starts a
	// different set of processes and does not publish the tunnel to the Worker,
	// so it must not win for a loopback relay. It is still honoured for a
	// remote relay, where this machine has no local Activity stack to run.
	if localActivity && script != "" {
		logger.Info("using the built-in Activity stack instead of the configured script", "script", script)
		script = ""
	}

	// Resolve Discord credentials for Activity stack
	discordClientID := strings.TrimSpace(opts.Relay.DiscordClientID)
	discordSecret := strings.TrimSpace(opts.Relay.DiscordClientSecret)
	identitySecret := strings.TrimSpace(opts.Relay.IdentitySecret)

	// If DiscordClientID not in config, try to read from viewer config.json
	if discordClientID == "" && opts.ViewerDir != "" {
		discordClientID = readDiscordClientIDFromViewerConfig(opts.ViewerDir)
	}

	// If DiscordClientSecret not in config, check environment variable
	if discordSecret == "" {
		discordSecret = strings.TrimSpace(os.Getenv("DISCORD_CLIENT_SECRET"))
	}

	// If IdentitySecret not in config, try to load from saved file, then generate
	if identitySecret == "" {
		identitySecret = loadIdentitySecret(opts.LogDir)
		if identitySecret == "" {
			identitySecret = generateIdentitySecret()
			// Save to config file for persistence
			saveIdentitySecret(opts.LogDir, identitySecret, logger)
		}
	}

	// Find cloudflared binary if cloudflared is enabled
	cloudflaredBin := ""
	if opts.Relay.Cloudflared {
		if path, err := exec.LookPath("cloudflared"); err == nil {
			cloudflaredBin = path
		} else {
			// Try common install locations
			for _, p := range []string{
				`C:\Program Files\cloudflared\cloudflared.exe`,
				`C:\Program Files (x86)\cloudflared\cloudflared.exe`,
			} {
				if _, err := os.Stat(p); err == nil {
					cloudflaredBin = p
					break
				}
			}
		}
		if cloudflaredBin == "" {
			logger.Warn("cloudflared not found in PATH or common locations; tunnel will not start")
		}
	}

	workerURL := deriveWorkerURL(opts.SignalingURL)
	// The Worker admin token is a separate secret from the relay's publish
	// token: the Worker's TUNNEL_ADMIN_TOKEN authorises /__tunnel, while the
	// publish token authorises WebSocket publishers. Reusing one for the other
	// is what made tunnel publishing answer 401 and left the front door
	// pointing at a dead tunnel.
	workerToken := strings.TrimSpace(os.Getenv("STREAMY_TUNNEL_ADMIN_TOKEN"))
	if workerToken == "" {
		workerToken = strings.TrimSpace(os.Getenv("TUNNEL_ADMIN_TOKEN"))
	}
	if saved := loadTunnelAdminToken(opts.LogDir); saved != "" {
		workerToken = saved
	} else if workerToken != "" {
		saveTunnelAdminToken(opts.LogDir, workerToken, logger)
	}

	m := &Manager{
		logger:            logger,
		mode:              "bot",
		script:            script,
		publishToken:      strings.TrimSpace(settings.PublishToken),
		port:              portFromNotifyURL(opts.NotifyURL),
		apiKey:            strings.TrimSpace(opts.NotifyKey),
		logDir:            opts.LogDir,
		logLevel:          opts.LogLevel,
		maxViewers:        opts.MaxViewers,
		executable:        strings.TrimSpace(opts.Executable),
		cloudflared:       opts.Relay.Cloudflared,
		cloudflaredBin:    cloudflaredBin,
		viewerDir:         strings.TrimSpace(opts.ViewerDir),
		discordClientID:   discordClientID,
		discordSecret:     discordSecret,
		identitySecret:    identitySecret,
		workerURL:         workerURL,
		workerToken:       workerToken,
	}
	if m.publishToken == "" {
		m.publishToken = strings.TrimSpace(opts.Relay.Token)
	}

	if localActivity {
		m.mode = "activity"
		m.gatewayAddr = gateway
		m.relayAddr = relay
		m.botAddr = bot
		m.healthAddr = healthAddr(gateway)
	}
	if m.healthAddr == "" {
		m.healthAddr = fmt.Sprintf("127.0.0.1:%d", m.port)
	}
	return m
}

// deriveWorkerURL converts the signaling WebSocket URL to the Worker HTTP URL.
// e.g., wss://streamy-signal.thenightserver.workers.dev -> https://streamy-signal.thenightserver.workers.dev
func deriveWorkerURL(signalingURL string) string {
	u, err := url.Parse(signalingURL)
	if err != nil {
		return ""
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
		return u.String()
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
		return u.String()
	}
	return signalingURL
}

// defaultActivityScript finds the packaged Activity launcher beside the app.
// A development checkout normally sets stack.script explicitly; an installed
// build ships stack\start-stack.ps1, and this makes it work without requiring
// the user to edit config.json.
func defaultActivityScript() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(filepath.Dir(self), "stack", "start-stack.ps1"),
		filepath.Join(filepath.Dir(self), "start-stack.ps1"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// readDiscordClientIDFromViewerConfig reads the discordClientID from the viewer's config.json.
func readDiscordClientIDFromViewerConfig(viewerDir string) string {
	// Try dist/config.json first (built), then public/config.json (source)
	for _, name := range []string{"dist/config.json", "public/config.json"} {
		path := filepath.Join(viewerDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg struct {
			DiscordClientID string `json:"discordClientID"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		if cfg.DiscordClientID != "" {
			return cfg.DiscordClientID
		}
	}
	return ""
}

// generateIdentitySecret generates a random 32-byte secret encoded as base64.
func generateIdentitySecret() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to a time-based seed if crypto/rand fails
		return fmt.Sprintf("streamy-identity-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}

// saveIdentitySecret saves the identity secret to a file in the log directory for persistence.
func saveIdentitySecret(logDir, secret string, logger *slog.Logger) {
	if logDir == "" {
		return
	}
	secretDir := filepath.Join(logDir, "stack")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		return
	}
	secretPath := filepath.Join(secretDir, "identity_secret.txt")
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		logger.Warn("failed to save identity secret", "error", err)
		return
	}
	logger.Debug("saved identity secret", "path", secretPath)
}

// loadIdentitySecret loads the identity secret from the saved file if it exists.
func loadIdentitySecret(logDir string) string {
	if logDir == "" {
		return ""
	}
	secretPath := filepath.Join(logDir, "stack", "identity_secret.txt")
	data, err := os.ReadFile(secretPath)
	if err != nil {
		return ""
	}
	secret := strings.TrimSpace(string(data))
	if secret != "" {
		return secret
	}
	return ""
}

// loadTunnelAdminToken reads the Worker admin secret saved beside the identity
// secret. It is optional: without it the stack still runs, it just cannot
// publish its cloudflared hostname to the Worker front door.
func loadTunnelAdminToken(logDir string) string {
	if logDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(logDir, "stack", "tunnel-admin-token.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveTunnelAdminToken persists a token supplied through the environment so the
// next launch (which may not inherit that environment) can publish the tunnel.
func saveTunnelAdminToken(logDir, token string, logger *slog.Logger) {
	if logDir == "" || token == "" {
		return
	}
	secretDir := filepath.Join(logDir, "stack")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		logger.Warn("failed to create the stack directory for the tunnel token", "error", err)
		return
	}
	secretPath := filepath.Join(secretDir, "tunnel-admin-token.txt")
	if err := os.WriteFile(secretPath, []byte(token), 0o600); err != nil {
		logger.Warn("failed to save the tunnel admin token", "error", err)
		return
	}
	logger.Debug("saved the tunnel admin token", "path", secretPath)
}

// Start launches the child in the background. It is safe to call repeatedly.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.generation++
	generation := m.generation
	script := m.script
	m.mu.Unlock()

	if script != "" {
		if info, err := os.Stat(script); err == nil && !info.IsDir() {
			m.logger.Info("starting the local stack from a script", "script", script)
			go m.runScript(script, generation)
			return
		}
		m.logger.Warn("configured stack script was not found; using the built-in stack", "script", script)
	}

	// A previous Streamy process may have left the stack running. Adopt it
	// instead of fighting over the port.
	if m.healthOK() {
		m.mu.Lock()
		if generation == m.generation {
			m.running = true
			m.owned = false
			m.err = nil
		}
		m.mu.Unlock()
		m.logger.Info(m.label()+" is already running", "health", m.healthAddr)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	if generation != m.generation {
		m.mu.Unlock()
		cancel()
		return
	}
	m.cancel = cancel
	m.procDone = done
	m.owned = true
	m.mu.Unlock()

	if m.mode == "activity" {
		m.logger.Info("starting the local Activity stack",
			"gateway", m.gatewayAddr, "relay", m.relayAddr, "bot", m.botAddr)
		go m.runChild(ctx, done, generation, m.activityArgs(), m.label())
		return
	}
	go m.runChild(ctx, done, generation,
		[]string{"bot", "-port", strconv.Itoa(m.port)}, m.label())
}

// Running reports whether the child is answering its health endpoint. The idle
// watchdog uses it to decide whether there is anything to stop.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Stop shuts the child down. A child started by this process is killed; an
// adopted notification bot is asked to exit gracefully. An adopted Activity
// stack is left alone because it has no quit endpoint and somebody else owns
// its lifetime.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	started := m.started
	script := m.script
	cancel := m.cancel
	done := m.procDone
	owned := m.owned
	mode := m.mode
	m.started = false
	m.running = false
	m.owned = false
	m.cancel = nil
	m.procDone = nil
	m.generation++
	m.mu.Unlock()

	if !started {
		return nil
	}

	if script != "" {
		if info, err := os.Stat(script); err == nil && !info.IsDir() {
			if cancel != nil {
				cancel()
			}
			m.logger.Info("stopping the local stack")
			runCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer stopCancel()
			args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Stop"}
			if err := m.exec(runCtx, args...); err != nil {
				return fmt.Errorf("stop local stack: %w", err)
			}
			return nil
		}
	}

	if mode == "activity" && !owned {
		m.logger.Info("the Activity stack was started outside Streamy; leaving it running")
		return nil
	}

	m.logger.Info("stopping " + m.label())
	if mode == "bot" {
		m.requestQuit(ctx)
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(stopTimeout):
			m.logger.Warn(m.label() + " did not stop in time")
		}
	}
	return nil
}

// runChild starts args as a child of this executable and waits until its health
// endpoint answers. It is shared by the notification bot and the Activity
// stack so both get the same crash handling and startup timeout.
func (m *Manager) runChild(ctx context.Context, done chan struct{}, generation int, args []string, label string) {
	defer close(done)

	self, err := m.self()
	if err != nil {
		m.finish(generation, err, false)
		return
	}
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Dir = filepath.Dir(self)
	executil.HideConsoleWindow(cmd)

	// Pass Discord credentials as environment variables (activity command reads from env as fallback)
	if m.discordClientID != "" {
		cmd.Env = append(cmd.Env, "DISCORD_CLIENT_ID="+m.discordClientID)
	}
	if m.discordSecret != "" {
		cmd.Env = append(cmd.Env, "DISCORD_CLIENT_SECRET="+m.discordSecret)
	}
	if m.identitySecret != "" {
		cmd.Env = append(cmd.Env, "DISCORD_IDENTITY_SECRET="+m.identitySecret)
	}
	// Also pass publish token for the relay
	if m.publishToken != "" {
		cmd.Env = append(cmd.Env, "DISCORD_PUBLISH_TOKEN="+m.publishToken)
	}

	if logFile := m.openLog(); logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}
	if err := cmd.Start(); err != nil {
		m.finish(generation, err, false)
		return
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	deadline := time.Now().Add(startupTimeout)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-waitCh:
			// The child exited before it became healthy: a bad token, a port
			// already in use, or a missing .env.
			if err != nil {
				m.logger.Warn(label+" exited during startup", "error", err)
			}
			m.finish(generation, err, false)
			return
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-waitCh
			m.finish(generation, ctx.Err(), false)
			return
		case <-ticker.C:
			if m.healthOK() {
				m.mu.Lock()
				if generation == m.generation {
					m.running = true
					m.err = nil
				}
				m.mu.Unlock()
				m.logger.Info(label+" is ready", "health", m.healthAddr)

				err := <-waitCh
				if err != nil && ctx.Err() == nil {
					m.logger.Warn(label+" exited", "error", err)
				}
				m.finish(generation, err, false)
				return
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				<-waitCh
				m.finish(generation, fmt.Errorf("%s did not become healthy on %s", label, m.healthAddr), false)
				return
			}
		}
	}
}

func (m *Manager) runScript(script string, generation int) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	m.mu.Lock()
	if generation != m.generation {
		m.mu.Unlock()
		cancel()
		return
	}
	m.cancel = cancel
	m.mu.Unlock()
	defer cancel()

	args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Hidden"}
	if m.publishToken != "" {
		args = append(args, "-PublishToken", m.publishToken)
	}
	err := m.exec(ctx, args...)
	m.finish(generation, err, err == nil)
	if err != nil {
		m.logger.Warn("the local stack did not start", "error", err)
		return
	}
	m.logger.Info("the local stack is ready")
}

func (m *Manager) finish(generation int, err error, running bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		return
	}
	m.running = running
	m.err = err
	if !running {
		// The child is gone. Clear started so the next Start (for example when
		// the user begins a new stream) relaunches it instead of leaving the
		// manager stuck after a failed startup or a crash.
		m.started = false
		m.owned = false
	}
}

func (m *Manager) exec(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "powershell.exe", args...)
	executil.HideConsoleWindow(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(output))
		if text != "" {
			return fmt.Errorf("%w: %s", err, tail(text, 800))
		}
		return err
	}
	return nil
}

func (m *Manager) healthOK() bool {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + m.healthAddr + "/healthz")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode >= 200 && response.StatusCode < 300
}

func (m *Manager) requestQuit(ctx context.Context) {
	if m.apiKey == "" {
		return
	}
	quitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(quitCtx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/quit", m.port), nil)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+m.apiKey)
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return
	}
	response.Body.Close()
}

// self returns the executable that hosts the bot and activity subcommands.
func (m *Manager) self() (string, error) {
	if m.executable != "" {
		return m.executable, nil
	}
	return os.Executable()
}

// activityArgs builds the `streamy activity` command line. The publish token is
// the relay's publisher secret (stream.stack.publishToken wins over
// stream.relay.token); the addresses were derived from stream.relay.url.
func (m *Manager) activityArgs() []string {
	args := []string{
		"activity",
		"-gateway", m.gatewayAddr,
		"-relay", m.relayAddr,
		"-bot", m.botAddr,
	}
	if m.publishToken != "" {
		args = append(args, "-publish-token", m.publishToken)
	}
	if m.maxViewers > 0 {
		args = append(args, "-max-viewers", strconv.Itoa(m.maxViewers))
	}
	if m.logLevel != "" {
		args = append(args, "-log-level", m.logLevel)
	}
	if m.cloudflared {
		args = append(args, "-cloudflared")
		if m.cloudflaredBin != "" {
			args = append(args, "-cloudflared-bin", m.cloudflaredBin)
		}
	}
	if m.viewerDir != "" {
		args = append(args, "-viewer-dir", m.viewerDir)
	}
	// Worker front door for tunnel publishing
	if m.workerURL != "" {
		args = append(args, "-worker-url", m.workerURL)
	}
	if m.workerToken != "" {
		args = append(args, "-worker-token", m.workerToken)
	}
	// Discord OAuth credentials for Activity
	if m.discordClientID != "" {
		args = append(args, "-discord-client", m.discordClientID)
	}
	if m.discordSecret != "" {
		args = append(args, "-discord-secret", m.discordSecret)
	}
	if m.identitySecret != "" {
		args = append(args, "-identity-secret", m.identitySecret)
	}
	return args
}

func (m *Manager) label() string {
	if m.mode == "activity" {
		return "the local Activity stack"
	}
	return "the local Discord bot"
}

func (m *Manager) openLog() *os.File {
	dir := ""
	if m.logDir != "" {
		dir = filepath.Join(m.logDir, "stack")
	} else {
		self, err := m.self()
		if err != nil {
			return nil
		}
		dir = filepath.Join(filepath.Dir(self), "stack", ".run")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	name := "bot.log"
	if m.mode == "activity" {
		name = "activity.log"
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return file
}

// localActivityAddresses decides whether the built-in Activity stack should run
// for this relay URL and, if so, which ports it should use. Only a plain ws URL
// on a loopback host can be served by the built-in plain-WebSocket stack; a
// remote or TLS relay is somebody else's deployment and must not spawn one.
//
// The port in the URL tells us which service the desktop app publishes to:
// ws://127.0.0.1:8080/.proxy/relay goes through the gateway, while
// ws://127.0.0.1:8081 talks to the relay directly.
func localActivityAddresses(enabled bool, raw string) (gateway, relay, bot string, ok bool) {
	if !enabled {
		return "", "", "", false
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "ws" {
		return "", "", "", false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
	default:
		return "", "", "", false
	}
	portNumber, err := strconv.Atoi(parsed.Port())
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", "", false
	}

	used := map[int]bool{}
	var gatewayPort, relayPort, botPort int
	if strings.Contains(parsed.Path, "/.proxy/relay") {
		gatewayPort = portNumber
		used[gatewayPort] = true
		relayPort = firstFreePort(used, defaultRelayPort, 18081)
		botPort = firstFreePort(used, defaultActivityBotPort, 18083)
	} else {
		relayPort = portNumber
		used[relayPort] = true
		gatewayPort = firstFreePort(used, defaultGatewayPort, 18080)
		botPort = firstFreePort(used, defaultActivityBotPort, 18083)
	}
	return fmt.Sprintf(":%d", gatewayPort),
		fmt.Sprintf(":%d", relayPort),
		fmt.Sprintf(":%d", botPort),
		true
}

// firstFreePort returns the first candidate port that is not already assigned
// to another service. The fallback list is deliberately short and predictable;
// a busy port still fails fast inside `streamy activity` with a clear error.
func firstFreePort(used map[int]bool, candidates ...int) int {
	for _, port := range candidates {
		if !used[port] {
			used[port] = true
			return port
		}
	}
	for port := 28080; ; port++ {
		if !used[port] {
			used[port] = true
			return port
		}
	}
}

// healthAddr turns a listen address into one that can be dialled locally.
func healthAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1" + listen
	}
	switch host {
	case "", "::", "0.0.0.0":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func portFromNotifyURL(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return defaultBotPort
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return defaultBotPort
	}
	if value := parsed.Port(); value != "" {
		if port, err := strconv.Atoi(value); err == nil && port > 0 && port < 65536 {
			return port
		}
	}
	return defaultBotPort
}

func tail(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[len(text)-max:]
}

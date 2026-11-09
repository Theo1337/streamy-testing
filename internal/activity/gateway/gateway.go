package gateway

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"streamy/internal/activity/bot"
	"streamy/internal/activity/relay"
)

//go:embed viewer/*
var viewerFS embed.FS

type Gateway struct {
	logger    *slog.Logger
	bot       *bot.Bot
	relay     *relay.Server
	viewerDir string

	httpServer *http.Server
}

func NewGateway(logger *slog.Logger, bot *bot.Bot, relay *relay.Server, viewerDir string) *Gateway {
	return &Gateway{
		logger:    logger,
		bot:       bot,
		relay:     relay,
		viewerDir: viewerDir,
	}
}

func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})

	// Config endpoint fallback if static viewer does not have config.json
	handleConfig := func(w http.ResponseWriter, r *http.Request) {
		if g.viewerDir != "" {
			p := filepath.Join(g.viewerDir, "config.json")
			if _, err := os.Stat(p); err == nil {
				http.ServeFile(w, r, p)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"signalURL":       "wss://streamy-signal.thenightserver.workers.dev",
			"relayURL":        "",
			"tokenURL":        "/.proxy/api/token",
			"downloadURL":     "https://github.com/Theo1337/streamy-app/releases",
			"discordClientID": g.bot.DiscordClientID(),
		})
	}
	mux.HandleFunc("GET /config.json", handleConfig)
	mux.HandleFunc("GET /.proxy/config.json", handleConfig)

	// Static viewer app. The pattern must not be method-specific: "/api/" and
	// "/.proxy/relay/" below match every method, and net/http rejects a
	// method-specific "/" that overlaps them.
	mux.Handle("/", g.staticViewer())

	// Bot API proxy. The bot serves the same /api prefix, so it is kept.
	botProxy := g.proxy("/api/", "/api/", g.botURL())
	mux.Handle("/api/", botProxy)
	// Discord's proxy strips the /.proxy prefix, so a mapped /api request
	// arrives as /token or /api/token. Serve both directly as well: this makes
	// the gateway usable as the Activity target without the Worker in front.
	mux.Handle("/token", g.proxy("/token", "/token", g.botURL()))
	mux.Handle("/.proxy/api/", g.proxy("/.proxy/api/", "/api/", g.botURL()))

	// Relay WebSocket proxy. There are four shapes in the wild, and all of
	// them must reach the relay:
	//
	//   /.proxy/relay/v1/watch/<room>  the Activity viewer, through Discord
	//   /relay/v1/watch/<room>         a URL mapping that strips only /relay
	//   /v1/watch/<room>               Discord strips both /.proxy and /relay
	//   /v1/watch/<room>               a direct browser / tunnel client
	//
	// The last two are the same path, which is why the relay's own routes are
	// mounted here directly; before this, a Worker that forwarded /v1/... hit
	// the static viewer and answered 404 (or a proxy answered 502), and the
	// Activity sat on "Reconnecting".
	mux.Handle("/.proxy/relay/", g.proxy("/.proxy/relay/", "/", g.relayURL()))
	mux.Handle("/relay/", g.proxy("/relay/", "/", g.relayURL()))
	mux.Handle("/v1/", g.proxy("/v1/", "/v1/", g.relayURL()))

	return g.logRequests(mux)
}

func (g *Gateway) staticViewer() http.Handler {
	if g.viewerDir != "" {
		return http.FileServer(http.Dir(g.viewerDir))
	}
	subFS, _ := fs.Sub(viewerFS, "viewer")
	return http.FileServer(http.FS(subFS))
}

func (g *Gateway) botURL() *url.URL {
	return &url.URL{Scheme: "http", Host: dialableAddr(g.bot.Addr())}
}

func (g *Gateway) relayURL() *url.URL {
	return &url.URL{Scheme: "http", Host: dialableAddr(g.relay.Addr())}
}

// dialableAddr turns a wildcard listen address such as ":8081" or "[::]:8081"
// into one the proxy can dial. Dialing the unspecified address happens to work
// on Windows but is not portable, and it is not a meaningful Host header.
func dialableAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "::", "0.0.0.0":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (g *Gateway) proxy(gatewayPrefix, upstreamPrefix string, target *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Remove hop-by-hop headers
		resp.Header.Del("Transfer-Encoding")
		return nil
	}
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
		// Map the public path onto the upstream path: /api/x -> /api/x (the
		// bot serves that prefix) and /.proxy/relay/x -> /x (Discord strips
		// /.proxy, and the relay serves /v1, /healthz and /ready at the root).
		// Exact paths such as /token must not gain a trailing slash, or the
		// bot's method-specific route misses them.
		rest := strings.TrimPrefix(req.URL.Path, gatewayPrefix)
		rest = strings.TrimPrefix(rest, "/")
		prefix := strings.TrimSuffix(upstreamPrefix, "/")
		switch {
		case rest == "" && prefix == "":
			req.URL.Path = "/"
		case rest == "":
			req.URL.Path = prefix
		default:
			req.URL.Path = prefix + "/" + rest
		}
		req.URL.RawPath = ""
	}
	// The prefix must only be removed here. Wrapping with http.StripPrefix
	// would strip it twice and every proxied request would miss its segment.
	return proxy
}

func (g *Gateway) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(lrw, r)
		g.logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.status,
			"latency_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (l *loggingResponseWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

// Hijack lets the reverse proxy take over the connection for a WebSocket
// upgrade. Without it the /relay proxy answers 502 ("can't switch protocols
// using non-Hijacker ResponseWriter") and no viewer can ever connect.
func (l *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := l.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("gateway: response writer %T does not support hijacking", l.ResponseWriter)
	}
	conn, readWriter, err := hijacker.Hijack()
	if err == nil {
		l.status = http.StatusSwitchingProtocols
	}
	return conn, readWriter, err
}

func (l *loggingResponseWriter) Flush() {
	if flusher, ok := l.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (l *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return l.ResponseWriter
}

func (g *Gateway) Run(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gateway listen %s: %w", addr, err)
	}
	return g.Serve(ctx, listener)
}

// Serve runs the gateway on an already-bound listener.
func (g *Gateway) Serve(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           g.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	g.httpServer = httpServer

	errCh := make(chan error, 1)
	go func() {
		g.logger.Info("streamy gateway listening", "addr", listener.Addr().String())
		errCh <- httpServer.Serve(listener)
	}()

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

func (g *Gateway) Addr() string {
	if g.httpServer != nil {
		return g.httpServer.Addr
	}
	return ""
}

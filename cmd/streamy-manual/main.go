// Command streamy-manual runs the phase 3 WebRTC test with manual SDP exchange.
//
// Run it from the repository root, then open the printed localhost URL in a
// browser. This command is temporary test signaling; phase 4 replaces it with
// the Cloudflare Worker client.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"streamy/internal/capture"
	"streamy/internal/config"
	"streamy/internal/logging"
	"streamy/internal/pipeline"
	"streamy/internal/rtc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "streamy-manual:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to config.json")
	viewerDir := flag.String("viewer-dir", "", "path to stream-site/viewer (auto-detected from common project directories)")
	listenAddr := flag.String("listen", "127.0.0.1:8787", "local address for the test viewer page")
	flag.Parse()

	capture.EnableDPIAwareness()

	cfgPath := *configPath
	if cfgPath == "" {
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		cfgPath = path
	}
	cfg, created, err := config.LoadFrom(cfgPath)
	if err != nil {
		return err
	}

	logger, closeLog, err := logging.Setup(logging.Options{
		Level: cfg.LogLevel,
		File:  filepath.Join(filepath.Dir(cfgPath), "streamy.log"),
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := closeLog(); err != nil {
			fmt.Fprintln(os.Stderr, "streamy-manual: closing log:", err)
		}
	}()
	slog.SetDefault(logger)
	if created {
		logger.Info("created default config", "path", cfgPath)
	}

	pageDir, err := locateViewer(*viewerDir)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		return fmt.Errorf("start local test viewer on %s: %w", *listenAddr, err)
	}
	server := &http.Server{
		Handler:           http.FileServer(http.Dir(pageDir)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Serve(listener)
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Warn("stopping local viewer server", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sender, err := rtc.NewSender(iceServers(cfg), logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := sender.Close(); err != nil {
			logger.Debug("closing WebRTC peer connection", "error", err)
		}
	}()

	offer, err := sender.CreateOffer(ctx)
	if err != nil {
		return err
	}
	fmt.Println("Open this test viewer in a browser:")
	fmt.Printf("  http://%s/\n\n", listener.Addr())
	fmt.Println("Paste the following complete offer into the viewer, create its answer,")
	fmt.Println("then paste the viewer's one-line JSON answer here:")
	fmt.Println("----- OFFER BEGIN -----")
	fmt.Print(offer)
	fmt.Println("----- OFFER END -----")

	type answerResult struct {
		answer string
		err    error
	}
	answerCh := make(chan answerResult, 1)
	go func() {
		answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
		answerCh <- answerResult{answer: answer, err: err}
	}()
	var answer string
	select {
	case <-ctx.Done():
		return nil
	case result := <-answerCh:
		answer, err = result.answer, result.err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read browser answer: %w", err)
	}
	if strings.TrimSpace(answer) == "" {
		return errors.New("no browser answer was pasted")
	}
	if err := sender.SetAnswer(answer); err != nil {
		return err
	}

	reader, writer := io.Pipe()
	recorder := pipeline.NewRecorder(pipeline.Options{
		Logger:      logger,
		Format:      "h264",
		Stdout:      writer,
		CloseStdout: true,
	})
	session, err := recorder.Start(ctx, pipeline.Request{Config: cfg})
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return fmt.Errorf("start screen capture: %w", err)
	}
	logger.Info("manual WebRTC stream started", "target", session.Description)
	fmt.Println("\nSharing is live. Press Ctrl+C to stop.")

	streamDone := make(chan error, 1)
	streamFailure := make(chan error, 1)
	go func() {
		streamDone <- sendAccessUnits(
			reader,
			sender,
			time.Second/time.Duration(cfg.Encoding.FPS),
			func(err error) { streamFailure <- err },
		)
	}()

	var runErr error
	streamFinished := false
	streamFailureReported := false
	select {
	case <-ctx.Done():
	case err := <-streamDone:
		streamFinished = true
		if !errors.Is(err, io.EOF) {
			runErr = fmt.Errorf("forward encoded video: %w", err)
		} else {
			runErr = errors.New("ffmpeg ended the H.264 stream unexpectedly")
		}
	case err := <-sender.Failure():
		runErr = err
	case err := <-streamFailure:
		runErr = fmt.Errorf("forward encoded video: %w", err)
		streamFailureReported = true
	case err := <-serverDone:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("test viewer server stopped: %w", err)
		}
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	if err := recorder.Stop(stopCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("stop screen capture: %w", err))
	}
	if !streamFinished {
		select {
		case err := <-streamDone:
			if err != nil && !errors.Is(err, io.EOF) && !streamFailureReported {
				runErr = errors.Join(runErr, fmt.Errorf("finish forwarding video: %w", err))
			}
		case <-stopCtx.Done():
			runErr = errors.Join(runErr, errors.New("timed out waiting for the video stream to stop"))
			_ = reader.Close()
		}
	}
	_ = reader.Close()
	return runErr
}

func locateViewer(configured string) (string, error) {
	candidates := []string{configured}
	if configured == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("find the current directory: %w", err)
		}
		candidates = []string{
			filepath.Join(cwd, "stream-site", "viewer"),
			filepath.Join(cwd, "..", "stream-site", "viewer"),
			filepath.Join(cwd, "viewer"),
		}
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(filepath.Join(absolute, "index.html"))
		if err == nil && !info.IsDir() {
			return absolute, nil
		}
	}
	return "", errors.New("viewer index.html not found; run from the project root, or set -viewer-dir")
}

func iceServers(cfg config.Config) []webrtc.ICEServer {
	servers := make([]webrtc.ICEServer, 0, len(cfg.Stream.STUNServers)+len(cfg.Stream.TURNServers))
	for _, url := range cfg.Stream.STUNServers {
		servers = append(servers, webrtc.ICEServer{URLs: []string{url}})
	}
	for _, server := range cfg.Stream.TURNServers {
		servers = append(servers, webrtc.ICEServer{
			URLs:       server.URLs,
			Username:   server.Username,
			Credential: server.Credential,
		})
	}
	return servers
}

func sendAccessUnits(input io.Reader, sender *rtc.Sender, duration time.Duration, onError func(error)) error {
	reader := rtc.NewAccessUnitReader(input)
	for {
		accessUnit, err := reader.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				onError(err)
				return errors.Join(err, reader.Drain())
			}
			return err
		}
		if err := sender.WriteSample(accessUnit, duration); err != nil {
			onError(err)
			return errors.Join(err, reader.Drain())
		}
	}
}

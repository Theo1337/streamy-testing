// Command streamy-capture is a diagnostic tool for the capture pipeline.
//
// The tray app hides the pipeline behind a single hotkey, which makes it a poor
// thing to debug with: when the wrong screen is recorded you cannot tell which
// step chose it. This command exposes the same pieces directly, so each one can
// be inspected on its own:
//
//	streamy-capture -list-monitors     what screens exist, and which DXGI
//	                                   output each one maps to
//	streamy-capture -list-windows      what windows a picker would offer
//	streamy-capture -target            what would be captured, and why
//	streamy-capture -encoders          probe every H.264 encoder
//	streamy-capture -print-command     the ffmpeg command line, without running
//	streamy-capture -duration 10s      record for ten seconds
//	streamy-capture -stdout | ffplay - stream the raw H.264 into a player
//
// It uses the same internal/capture and internal/encode code the tray app does,
// so anything it gets wrong the app gets wrong too.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"streamy/internal/capture"
	"streamy/internal/config"
	"streamy/internal/encode"
	"streamy/internal/pipeline"
	"streamy/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "streamy-capture:", err)
		os.Exit(1)
	}
}

type options struct {
	configPath string

	listMonitors bool
	listWindows  bool
	listPaths    bool
	showTarget   bool
	listEncoders bool
	printCommand bool
	useRecorder  bool

	outputPath string
	toStdout   bool
	duration   time.Duration

	fps      int
	bitrate  int
	scale    string
	encoder  string
	format   string
	grabber  string
	monitor  int
	window   string
	mouse    bool
	mouseSet bool
	titles   bool
	verbose  bool
}

func run() error {
	// Must happen before the first window exists, or Windows scales the
	// coordinates it reports and the fullscreen detection is wrong on any
	// display not running at 100%.
	capture.EnableDPIAwareness()

	var opts options
	flag.StringVar(&opts.configPath, "config", "", "path to config.json (default: the user config directory)")
	flag.BoolVar(&opts.listMonitors, "list-monitors", false, "print the monitors and the DXGI output each maps to")
	flag.BoolVar(&opts.listWindows, "list-windows", false, "print the windows a picker would offer")
	flag.BoolVar(&opts.listPaths, "list-paths", false, "print the raw display configuration paths (adapter, target id, rectangle, rotation)")
	flag.BoolVar(&opts.showTarget, "target", false, "print what would be captured and why, then exit")
	flag.BoolVar(&opts.listEncoders, "encoders", false, "probe every H.264 encoder and print the result")
	flag.BoolVar(&opts.printCommand, "print-command", false, "print the ffmpeg command line instead of running it")
	flag.BoolVar(&opts.useRecorder, "recorder", false, "record through pipeline.Recorder, the object the tray app uses")
	flag.StringVar(&opts.outputPath, "o", "", "write the recording here (default: a temporary file)")
	flag.BoolVar(&opts.toStdout, "stdout", false, "write the raw H.264 stream to stdout")
	flag.DurationVar(&opts.duration, "duration", 5*time.Second, "how long to record; 0 means until Ctrl+C")
	flag.IntVar(&opts.fps, "fps", 0, "frame rate (default: from the config)")
	flag.StringVar(&opts.scale, "scale", "", "resize the capture, for example 1280x720")
	flag.IntVar(&opts.bitrate, "bitrate", 0, "video bitrate in kbps (default: from the config)")
	flag.StringVar(&opts.encoder, "encoder", "", "auto, nvenc, qsv, amf or x264 (default: from the config)")
	flag.StringVar(&opts.format, "format", "", "mp4, mkv, mp4-frag or h264 (default: mp4)")
	flag.StringVar(&opts.grabber, "grabber", "", "auto, ddagrab or gdigrab (default: probe for ddagrab)")
	flag.IntVar(&opts.monitor, "monitor", -1, "capture this monitor by index instead of auto-selecting")
	flag.StringVar(&opts.window, "window", "", "capture a window whose title contains this text")
	flag.BoolVar(&opts.mouse, "mouse", true, "draw the mouse pointer into the capture")
	flag.BoolVar(&opts.titles, "titles", false, "print window titles in -list-windows (they are whatever you happen to have open)")
	flag.BoolVar(&opts.verbose, "v", false, "show ffmpeg's own diagnostics")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("streamy-capture", version.String())
		return nil
	}
	opts.mouseSet = true

	logger := newLogger(opts.verbose)

	cfg, err := loadConfig(opts.configPath, logger)
	if err != nil {
		return err
	}
	// Apply the flag overrides on top of the config, so the debug tool defaults
	// to exactly what the app would do.
	if opts.fps > 0 {
		cfg.Encoding.FPS = opts.fps
	}
	if opts.bitrate > 0 {
		cfg.Encoding.BitrateKbps = opts.bitrate
	}
	if opts.scale != "" {
		cfg.Encoding.Resolution = opts.scale
	}
	if opts.encoder != "" {
		cfg.Encoding.Encoder = opts.encoder
	}
	if opts.verbose {
		cfg.LogLevel = "debug"
	}

	// A recording has to go somewhere. Defaulting to a temporary file keeps
	// this tool honest: -print-command shows a real path, and a plain run
	// leaves a file behind that can actually be opened.
	if !opts.toStdout && opts.outputPath == "" {
		opts.outputPath = filepath.Join(os.TempDir(),
			"streamy-capture-"+time.Now().Format("20060102-150405")+".mp4")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resolver := capture.NewResolver(logger)

	switch {
	case opts.listMonitors:
		return listMonitors(ctx, resolver)
	case opts.listWindows:
		return listWindows(ctx, resolver, opts.titles)
	case opts.listPaths:
		return listPaths()
	case opts.showTarget:
		return showTarget(ctx, resolver, cfg)
	case opts.listEncoders:
		return listEncoders(ctx, cfg, logger)
	case opts.useRecorder:
		return recordViaRecorder(ctx, cfg, logger, opts)
	default:
		return record(ctx, cfg, logger, resolver, opts)
	}
}

// newLogger writes to stderr so that -stdout stays clean.
func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// loadConfig reads the config, falling back to the defaults when there is none.
func loadConfig(path string, logger *slog.Logger) (config.Config, error) {
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return config.Default(), nil
		}
		path = p
	}
	cfg, _, err := config.LoadFrom(path)
	if err != nil {
		// A broken config should not stop a diagnostic tool from running; say
		// so and carry on with the defaults.
		logger.Warn("could not read the config, using the defaults", "path", path, "error", err)
		return config.Default(), nil
	}
	logger.Debug("loaded the config", "path", path)
	return cfg, nil
}

func listMonitors(ctx context.Context, resolver capture.TargetResolver) error {
	d, err := resolver.Snapshot(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("%d monitors\n", len(d.Monitors))
	for _, m := range d.Monitors {
		primary := " "
		if m.Primary {
			primary = "*"
		}
		dxgi := "none"
		if m.DXGIIndex >= 0 {
			dxgi = fmt.Sprintf("%d", m.DXGIIndex)
		}
		fmt.Printf("%s [%d] %-14s bounds=%-18s work=%-18s dxgi=%-4s rotation=%-2d signal=%dx%d\n",
			primary, m.Index, m.Name, m.Bounds, m.WorkArea, dxgi, m.Rotation, m.SignalSize.Width, m.SignalSize.Height)
	}

	if d.MouseMonitor >= 0 {
		fmt.Printf("the pointer is on monitor %d\n", d.MouseMonitor)
	}
	// Deliberately no window title here. A title is whatever the user happens
	// to have open, and this command's output is pasted into bug reports and
	// logs; -list-windows shows titles when they are actually wanted.
	if d.Foreground != nil {
		fmt.Printf("a capturable foreground window exists: class %q, rect %s, pid %d\n",
			d.Foreground.Class, d.Foreground.Rect, d.Foreground.PID)
	} else {
		fmt.Println("no capturable foreground window")
	}
	return nil
}

// windowLister is the optional interface the Windows resolver adds for the
// picker. The core TargetResolver does not need it, so it is asserted for
// rather than required.
type windowLister interface {
	Windows(ctx context.Context) ([]capture.Window, error)
}

func listPaths() error {
	paths, err := capture.DisplayPaths()
	if err != nil {
		return err
	}

	fmt.Printf("%d active display paths\n", len(paths))
	for i, p := range paths {
		fmt.Printf("  path %d: adapterLuid=0x%016X targetId=%-6d desktop=%-18s active=%-12s rotation=%d tech=%d\n",
			i, p.AdapterLUID, p.TargetID, p.Desktop, p.ActiveSize, p.Rotation, p.OutputTechnology)
	}
	return nil
}

func listWindows(ctx context.Context, resolver capture.TargetResolver, showTitles bool) error {
	lister, ok := resolver.(windowLister)
	if !ok {
		return fmt.Errorf("listing windows is not implemented on this platform")
	}
	windows, err := lister.Windows(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("%d windows\n", len(windows))
	for _, w := range windows {
		// Titles are opt-in: they are whatever the user has open, and this
		// command's output tends to end up in a log or a bug report.
		title := ""
		if showTitles {
			title = " " + strconv.Quote(truncate(w.Title, 70))
		}
		fmt.Printf("  pid=%-6d %-24s %-18s len(title)=%d%s\n",
			w.PID, truncate(w.Class, 24), w.Rect, len([]rune(w.Title)), title)
	}
	return nil
}

func showTarget(ctx context.Context, resolver capture.TargetResolver, cfg config.Config) error {
	d, err := resolver.Snapshot(ctx)
	if err != nil {
		return err
	}
	target, err := capture.Decide(d, cfg)
	if err != nil {
		return err
	}

	fmt.Printf("would capture: %s\n", target.Describe())
	fmt.Printf("kind:         %s\n", target.Kind)
	fmt.Printf("fullscreen:   %t\n", target.Fullscreen)
	fmt.Printf("cursor:       %t\n", target.Cursor)
	fmt.Printf("reason:       %s\n", target.Reason)
	return nil
}

func listEncoders(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ffmpeg, err := encode.Find(ctx, cfg.Encoding.FFmpegPath)
	if err != nil {
		return err
	}
	fmt.Printf("ffmpeg:  %s\n%s\n\n", ffmpeg.Path, ffmpeg.Version())

	// Every candidate is probed, not just the winner, because the interesting
	// part of this command's output is the reason each one failed.
	fmt.Println("probing every H.264 encoder with the real encoder arguments:")
	for _, enc := range encode.PreferenceOrder {
		start := time.Now()
		err := ffmpeg.Probe(ctx, cfg, enc)
		elapsed := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Printf("  %-12s FAIL (%s)\n      %v\n", enc.Name, elapsed, oneLine(err.Error()))
			continue
		}
		fmt.Printf("  %-12s ok   (%s)\n", enc.Name, elapsed)
	}

	fmt.Println()
	selected, err := encode.Select(ctx, ffmpeg, cfg, logger)
	if err != nil {
		return err
	}
	fmt.Printf("encoding.encoder is %q, so the choice is: %s\n", cfg.Encoding.Encoder, selected)

	start := time.Now()
	if err := ffmpeg.ProbeDDAGrab(ctx); err != nil {
		fmt.Printf("Desktop Duplication (ddagrab): FAIL (%s)\n      %v\n", time.Since(start).Round(time.Millisecond), oneLine(err.Error()))
		fmt.Println("      fullscreen captures will fall back to GDI, which can record black")
	} else {
		fmt.Printf("Desktop Duplication (ddagrab): ok   (%s)\n", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// recordViaRecorder drives pipeline.Recorder, the object the tray app uses, so
// the app-facing path can be smoke tested without pressing the hotkey.
func recordViaRecorder(ctx context.Context, cfg config.Config, logger *slog.Logger, opts options) error {
	recorderOpts := pipeline.Options{Logger: logger, Format: opts.format}
	if opts.toStdout {
		recorderOpts.Stdout = os.Stdout
	} else if dir := dirOf(opts.outputPath); dir != "" {
		recorderOpts.OutputDir = dir
		// The recorder names each file from the moment it started, because the
		// app never asks the user for a filename. Only the directory is ours to
		// choose, so say so rather than quietly ignoring the rest of the path.
		fmt.Fprintf(infoW(opts.toStdout),
			"note: the recorder names its own file; only the directory from -o is used\n")
	}

	recorder := pipeline.NewRecorder(recorderOpts)

	req := pipeline.Request{Config: cfg}
	if opts.window != "" || opts.monitor >= 0 {
		target, err := chooseTarget(ctx, capture.NewResolver(logger), cfg, opts)
		if err != nil {
			return err
		}
		req.Target = &target
	}

	start := time.Now()
	session, err := recorder.Start(ctx, req)
	if err != nil {
		return err
	}
	info := infoW(opts.toStdout)
	fmt.Fprintf(info, "%s\n", session.Description)
	fmt.Fprintf(info, "link: %s\n", session.ViewerURL)

	waitFor(ctx, opts.duration)

	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = recorder.Stop(stopCtx)
	report(opts.toStdout, session.LocalRecording, time.Since(start))
	return err
}

func record(ctx context.Context, cfg config.Config, logger *slog.Logger, resolver capture.TargetResolver, opts options) error {
	ffmpeg, err := encode.Find(ctx, cfg.Encoding.FFmpegPath)
	if err != nil {
		return err
	}
	logger.Debug("using ffmpeg", "path", ffmpeg.Path, "version", ffmpeg.Version())

	encoder, err := encode.Select(ctx, ffmpeg, cfg, logger)
	if err != nil {
		return err
	}

	capabilities, err := capabilities(ctx, ffmpeg, cfg, opts, logger)
	if err != nil {
		return err
	}

	target, err := chooseTarget(ctx, resolver, cfg, opts)
	if err != nil {
		return err
	}
	grabber := target.ChooseGrabber(capabilities)

	output := encode.Output{Path: opts.outputPath, Format: opts.format, Pipe: opts.toStdout}

	args, err := encode.Args(cfg, target, encoder, capabilities, output)
	if err != nil {
		return err
	}

	info := infoW(opts.toStdout)
	fmt.Fprintf(info, "ffmpeg:  %s\n", ffmpeg.Path)
	fmt.Fprintf(info, "target:  %s\n", target.Describe())
	fmt.Fprintf(info, "reason:  %s\n", target.Reason)
	fmt.Fprintf(info, "grabber: %s\n", grabber)
	fmt.Fprintf(info, "encoder: %s\n", encoder)

	if opts.printCommand {
		fmt.Fprintf(info, "\ncommand:\n  %s\n", formatCommand(ffmpeg.Path, args, opts.titles))
		return nil
	}

	start := time.Now()
	runner, err := encode.Start(ctx, encode.StartOptions{
		FFmpeg:       ffmpeg,
		Config:       cfg,
		Target:       target,
		Encoder:      encoder,
		Capabilities: capabilities,
		Output:       output,
		Logger:       logger,
		Stderr:       stderrSink(opts),
	})
	if err != nil {
		return err
	}

	// ffmpeg reports a bad capture device by exiting almost at once. Saying
	// "recording" before checking would be a lie, and waiting for a duration
	// would just delay the bad news.
	if err := runner.WaitForStart(900 * time.Millisecond); err != nil {
		_ = runner.Stop(context.Background())
		return err
	}

	if output.Pipe {
		go func() {
			if _, err := io.Copy(os.Stdout, runner.Stdout()); err != nil {
				logger.Debug("copying the stream to stdout", "error", err)
			}
		}()
	}

	fmt.Fprintf(info, "recording for %s...\n", opts.duration)
	waitFor(ctx, opts.duration)

	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = runner.Stop(stopCtx)
	report(opts.toStdout, opts.outputPath, time.Since(start))
	return err
}

// waitFor blocks until the duration elapses, the context is cancelled, or there
// is no duration to wait for.
func waitFor(ctx context.Context, d time.Duration) {
	if d <= 0 {
		<-ctx.Done()
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// infoW is where human-readable progress goes.
//
// With -stdout the encoded H.264 stream owns stdout, so everything else has to
// go to stderr. Printing "recording for 5s" into the middle of a video stream
// is the kind of bug that shows up as a corrupt frame and no explanation.
func infoW(toStdout bool) io.Writer {
	if toStdout {
		return os.Stderr
	}
	return os.Stdout
}

// report prints what was produced, which is the point of recording at all.
func report(toStdout bool, path string, elapsed time.Duration) {
	if toStdout {
		fmt.Fprintf(os.Stderr, "done after %s\n", elapsed.Round(time.Millisecond))
		return
	}
	if path == "" {
		fmt.Fprintf(os.Stderr, "done after %s\n", elapsed.Round(time.Millisecond))
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "done after %s, but the recording is missing: %v\n", elapsed.Round(time.Millisecond), err)
		return
	}
	fmt.Fprintf(os.Stderr, "done after %s: %s (%.1f MB)\n",
		elapsed.Round(time.Millisecond), path, float64(info.Size())/(1024*1024))
}

// stderrSink decides whether ffmpeg's own diagnostics reach the console.
func stderrSink(opts options) io.Writer {
	if opts.verbose {
		return os.Stderr
	}
	return nil
}

// capabilities works out whether Desktop Duplication is usable here.
func capabilities(ctx context.Context, ffmpeg encode.FFmpeg, cfg config.Config, opts options, logger *slog.Logger) (capture.Capabilities, error) {
	switch strings.ToLower(opts.grabber) {
	case "ddagrab":
		// Trust the caller: if they name it, use it, and let the failure be
		// theirs. This is what makes it possible to test a failing grabber.
		// Return a map with a sentinel key meaning "all outputs".
		return capture.Capabilities{DDAGrab: map[int]int8{-1: capture.DDAGrabHealthy}}, nil

	case "gdigrab":
		return capture.Capabilities{DDAGrab: map[int]int8{}}, nil

	case "", "auto":
		start := time.Now()
		if err := ffmpeg.ProbeDDAGrab(ctx); err != nil {
			logger.Warn("ddagrab is unavailable, falling back to GDI", "error", err)
			return capture.Capabilities{DDAGrab: map[int]int8{}}, nil
		}
		logger.Debug("ddagrab is available", "probe", time.Since(start).Round(time.Millisecond))
		// Return a map with sentinel key meaning "all outputs healthy"
		return capture.Capabilities{DDAGrab: map[int]int8{-1: capture.DDAGrabHealthy}}, nil

	default:
		return capture.Capabilities{}, fmt.Errorf("unknown -grabber %q (want auto, ddagrab or gdigrab)", opts.grabber)
	}
}

// chooseTarget picks what to record, honouring -monitor and -window.
func chooseTarget(ctx context.Context, resolver capture.TargetResolver, cfg config.Config, opts options) (capture.Target, error) {
	if opts.window != "" {
		return windowTarget(ctx, resolver, opts.window)
	}
	if opts.monitor >= 0 {
		d, err := resolver.Snapshot(ctx)
		if err != nil {
			return capture.Target{}, err
		}
		if opts.monitor >= len(d.Monitors) {
			return capture.Target{}, fmt.Errorf("there is no monitor %d; this machine has %d", opts.monitor, len(d.Monitors))
		}
		m := d.Monitors[opts.monitor]
		return capture.Target{
			Kind:    capture.KindMonitor,
			Monitor: m,
			Cursor:  opts.mouse,
			Reason:  fmt.Sprintf("-monitor %d was given", opts.monitor),
		}, nil
	}
	return resolver.Resolve(ctx, cfg)
}

// windowTarget finds a window by a substring of its title, the same way gdigrab
// does, so a title with awkward characters can be checked before it is handed
// to ffmpeg.
func windowTarget(ctx context.Context, resolver capture.TargetResolver, title string) (capture.Target, error) {
	lister, ok := resolver.(windowLister)
	if !ok {
		return capture.Target{}, fmt.Errorf("capturing a window by title is not implemented on this platform")
	}
	windows, err := lister.Windows(ctx)
	if err != nil {
		return capture.Target{}, err
	}

	needle := strings.ToLower(title)
	var matches []capture.Window
	for _, w := range windows {
		if strings.Contains(strings.ToLower(w.Title), needle) {
			matches = append(matches, w)
		}
	}

	switch len(matches) {
	case 0:
		return capture.Target{}, fmt.Errorf("no window title contains %q; run -list-windows to see what is open", title)
	case 1:
		w := matches[0]
		target := capture.Target{
			Kind:   capture.KindWindow,
			Window: w,
			Cursor: true,
			Reason: fmt.Sprintf("-window %q matched %q", title, w.Title),
		}
		// Attaching the window's monitor lets ffmpeg duplicate that monitor and
		// crop the window out, which is both cheaper than GDI and immune to the
		// black picture GDI returns for GPU-composited windows.
		if desktop, err := resolver.Snapshot(ctx); err == nil {
			target = target.WithDesktop(desktop)
		}
		return target, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d windows, so it is ambiguous:\n", title, len(matches))
		for _, w := range matches {
			fmt.Fprintf(&b, "  %q\n", w.Title)
		}
		return capture.Target{}, fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
	}
}

// formatCommand renders an argument list the way it could be pasted into a
// shell, quoting anything with a space in it.
//
// A windowed capture used to pass the window's title to gdigrab, because that
// was the only way gdigrab could select a window. It is captured as a desktop
// rectangle now, but the redaction stays: a title is whatever the user has open,
// and no future argument should be assumed to be safe to print.
func formatCommand(exe string, args []string, showTitles bool) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteArg(exe))
	for _, a := range args {
		if !showTitles && strings.HasPrefix(a, "title=") {
			a = fmt.Sprintf("title=<%d character window title, hidden; add -titles to show it>",
				len([]rune(strings.TrimPrefix(a, "title="))))
		}
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "\u2026"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ;; ")
	return strings.TrimSpace(s)
}

func dirOf(path string) string {
	if path == "" {
		return ""
	}
	idx := strings.LastIndexAny(path, `/\`)
	if idx < 0 {
		return ""
	}
	return path[:idx]
}

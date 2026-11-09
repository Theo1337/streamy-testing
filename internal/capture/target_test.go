package capture

import (
	"strings"
	"testing"

	"streamy/internal/config"
)

// These tests cover the target selection rules. They need no desktop, no game
// and no Windows: they feed Decide a hand written Desktop and check the Target
// that comes back, which is exactly the split the package is built around.

func monitor(index, dxgi int, x, y, w, h int, primary bool) Monitor {
	return Monitor{
		Index:     index,
		DXGIIndex: dxgi,
		Bounds:    Rect{X: x, Y: y, Width: w, Height: h},
		WorkArea:  Rect{X: x, Y: y, Width: w, Height: h - 48},
		Primary:   primary,
		Name:      "screen",
	}
}

// desktop returns the three monitor layout that was on the development machine:
// a 1920x1080 screen to the left, a portrait 1080x1920 screen to the right, and
// the primary 1920x1080 in the middle.
func desktop() Desktop {
	return Desktop{
		Monitors: []Monitor{
			monitor(0, 0, -1920, 293, 1920, 1080, false),
			monitor(1, 1, 1920, -232, 1080, 1920, false),
			monitor(2, 2, 0, 0, 1920, 1080, true),
		},
		MouseMonitor: -1,
		SelfPID:      4242,
	}
}

func window(title, class string, pid uint32, r Rect) *Window {
	return &Window{
		Title:   title,
		Class:   class,
		PID:     pid,
		Rect:    r,
		Visible: true,
	}
}

func TestRectCoveredBy(t *testing.T) {
	screen := Rect{Width: 1920, Height: 1080}

	cases := []struct {
		name      string
		other     Rect
		tolerance int
		want      bool
	}{
		{"exact", Rect{Width: 1920, Height: 1080}, 8, true},
		{"larger than the screen", Rect{X: -2, Y: -2, Width: 1924, Height: 1084}, 8, true},
		{"a couple of pixels short", Rect{Width: 1918, Height: 1078}, 8, true},
		{"taskbar still visible", Rect{Width: 1920, Height: 1032}, 8, false},
		{"half the screen", Rect{Width: 960, Height: 1080}, 8, false},
		{"offset by a lot", Rect{X: 400, Width: 1920, Height: 1080}, 8, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := screen.CoveredBy(c.other, c.tolerance); got != c.want {
				t.Errorf("CoveredBy(%v, %d) = %v, want %v", c.other, c.tolerance, got, c.want)
			}
		})
	}
}

func TestDecideFullscreenWindowCapturesItsMonitor(t *testing.T) {
	d := desktop()
	// A game on the third monitor: a different monitor from the primary, so a
	// bug that always picks the primary is visible.
	d.Foreground = window("Some Game", "UnityWndClass", 999, Rect{Width: 1920, Height: 1080})

	target, err := Decide(d, config.Default())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if target.Kind != KindMonitor {
		t.Errorf("Kind = %v, want monitor", target.Kind)
	}
	if !target.Fullscreen {
		t.Error("Fullscreen = false, want true")
	}
	if target.Monitor.Index != 2 {
		t.Errorf("Monitor.Index = %d, want 2", target.Monitor.Index)
	}
	if target.Monitor.DXGIIndex != 2 {
		t.Errorf("Monitor.DXGIIndex = %d, want 2", target.Monitor.DXGIIndex)
	}
	if target.Reason == "" {
		t.Error("Reason is empty; every decision must explain itself")
	}
}

func TestDecideFullscreenOnSecondaryMonitor(t *testing.T) {
	d := desktop()
	// Fullscreen on the left hand monitor.
	d.Foreground = window("Video", "mpv", 999, Rect{X: -1920, Y: 293, Width: 1920, Height: 1080})

	target, err := Decide(d, config.Default())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Monitor.Index != 0 || target.Monitor.DXGIIndex != 0 {
		t.Errorf("got monitor %d (dxgi %d), want monitor 0 (dxgi 0)", target.Monitor.Index, target.Monitor.DXGIIndex)
	}
}

func TestDecideMaximisedWindowIsNotFullscreen(t *testing.T) {
	// This is the case FullscreenTolerance exists for. A maximised window is
	// the work area: full width but 48 pixels short of the bottom.
	d := desktop()
	d.Foreground = window("Notepad", "Notepad", 999, Rect{Width: 1920, Height: 1032})

	target, err := Decide(d, config.Default())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Fullscreen {
		t.Errorf("a maximised window was treated as fullscreen: %s", target.Reason)
	}
	if target.Kind != KindMonitor || !target.Monitor.Primary {
		t.Errorf("got %s, want the primary monitor", target.Describe())
	}
}

func TestDecideFocusedWindow(t *testing.T) {
	d := desktop()
	d.Foreground = window("Notes - Editor", "Notepad", 999, Rect{X: 100, Y: 100, Width: 800, Height: 600})

	cfg := config.Default()
	cfg.Capture.DefaultTarget = config.TargetFocused

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Kind != KindWindow {
		t.Fatalf("Kind = %v, want window", target.Kind)
	}
	if target.Window.Title != "Notes - Editor" {
		t.Errorf("Window.Title = %q", target.Window.Title)
	}
	// The monitor is recorded too, because gdigrab needs it for nothing but
	// logging does.
	if target.Monitor.Index != 2 {
		t.Errorf("Monitor.Index = %d, want 2", target.Monitor.Index)
	}
}

func TestDecideExplicitMonitor(t *testing.T) {
	d := desktop()

	cfg := config.Default()
	cfg.Capture.DefaultTarget = config.TargetMonitor
	cfg.Capture.MonitorIndex = 1

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Monitor.Index != 1 {
		t.Errorf("Monitor.Index = %d, want 1", target.Monitor.Index)
	}
}

func TestDecideMonitorIndexOutOfRangeFallsBackToPrimary(t *testing.T) {
	d := desktop()

	cfg := config.Default()
	cfg.Capture.DefaultTarget = config.TargetMonitor
	cfg.Capture.MonitorIndex = 7

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !target.Monitor.Primary {
		t.Errorf("got monitor %d, want the primary", target.Monitor.Index)
	}
	if target.Reason == "" {
		t.Error("the fallback must say so in Reason")
	}
}

// TestDecideIgnoresUnusableWindows walks the exclusion rules. Each case is a
// foreground window that must not become the target even when it fills its
// monitor.
func TestDecideIgnoresUnusableWindows(t *testing.T) {
	full := Rect{Width: 1920, Height: 1080}

	cases := []struct {
		name string
		win  *Window
	}{
		{"the desktop itself", window("Program Manager", "Progman", 999, full)},
		{"the wallpaper host", window("", "WorkerW", 999, full)},
		{"the taskbar", window("", "Shell_TrayWnd", 999, full)},
		{"a secondary taskbar", window("", "Shell_SecondaryTrayWnd", 999, full)},
		{"our own tray window", window("Streamy", "StreamyTray", 4242, full)},
		{"an untitled window", window("   ", "SomeClass", 999, full)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := desktop()
			d.Foreground = c.win

			target, err := Decide(d, config.Default())
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if target.Fullscreen {
				t.Errorf("%s was treated as a fullscreen app: %s", c.name, target.Reason)
			}
			if !target.Monitor.Primary {
				t.Errorf("%s: got %s, want the primary monitor", c.name, target.Describe())
			}
		})
	}
}

func TestDecideHiddenOrMinimisedForegroundIsIgnored(t *testing.T) {
	cases := []struct {
		name string
		win  *Window
	}{
		{"minimised", &Window{Title: "Game", Class: "Game", PID: 999, Rect: Rect{Width: 1920, Height: 1080}, Visible: true, Minimized: true}},
		{"hidden", &Window{Title: "Game", Class: "Game", PID: 999, Rect: Rect{Width: 1920, Height: 1080}, Visible: false}},
		{"zero sized", &Window{Title: "Game", Class: "Game", PID: 999, Visible: true}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := desktop()
			d.Foreground = c.win

			target, err := Decide(d, config.Default())
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if target.Fullscreen {
				t.Errorf("%s: treated as fullscreen: %s", c.name, target.Reason)
			}
		})
	}
}

func TestDecideNoForegroundWindow(t *testing.T) {
	d := desktop()
	d.Foreground = nil

	target, err := Decide(d, config.Default())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !target.Monitor.Primary {
		t.Errorf("got %s, want the primary monitor", target.Describe())
	}
}

func TestDecideAlwaysShowPickerIsNoted(t *testing.T) {
	// The picker arrives in phase 6. Until then the setting must not silently
	// do nothing: it says so in the reason, which is logged.
	d := desktop()

	cfg := config.Default()
	cfg.Capture.AlwaysShowPicker = true

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Reason == "" {
		t.Fatal("Reason is empty")
	}
	if !contains(target.Reason, "alwaysShowPicker") {
		t.Errorf("Reason %q does not mention the ignored setting", target.Reason)
	}
}

func TestDecideNoMonitors(t *testing.T) {
	if _, err := Decide(Desktop{}, config.Default()); err == nil {
		t.Fatal("Decide with no monitors returned no error")
	}
}

func TestMonitorAtPoint(t *testing.T) {
	d := desktop()

	cases := []struct {
		name string
		x, y int
		want int
	}{
		{"primary", 100, 100, 2},
		{"left hand monitor", -1800, 400, 0},
		{"portrait monitor", 2000, 100, 1},
		// A point below the left hand monitor is in the gap between screens,
		// so the nearest monitor wins.
		{"in the gap", -1800, 2000, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := monitorAtPoint(d.Monitors, c.x, c.y); got != c.want {
				t.Errorf("monitorAtPoint(%d, %d) = %d, want %d", c.x, c.y, got, c.want)
			}
		})
	}
}

func TestChooseGrabber(t *testing.T) {
	mon := Target{Kind: KindMonitor}
	monWithoutOutput := Target{
		Kind:    KindMonitor,
		Monitor: Monitor{Index: 1, DXGIIndex: -1, Bounds: Rect{X: -1920, Width: 1920, Height: 1080}},
	}
	onScreen := Target{
		Kind:    KindWindow,
		Window:  Window{Rect: Rect{X: 100, Y: 100, Width: 800, Height: 600}},
		Monitor: Monitor{DXGIIndex: 0, Bounds: Rect{Width: 1920, Height: 1080}},
	}
	offScreen := Target{Kind: KindWindow, Window: Window{Rect: Rect{X: 100, Y: 100, Width: 800, Height: 600}}}

	withDDA := Capabilities{DDAGrab: map[int]int8{-1: DDAGrabHealthy}}
	withoutDDA := Capabilities{DDAGrab: map[int]int8{}}

	if got := mon.ChooseGrabber(withDDA); got != GrabberDDAGrab {
		t.Errorf("monitor with ddagrab available: %q, want %q", got, GrabberDDAGrab)
	}
	if got := mon.ChooseGrabber(withoutDDA); got != GrabberGDigrab {
		t.Errorf("monitor without ddagrab: %q, want %q", got, GrabberGDigrab)
	}
	// joinMonitors leaves DXGIIndex at -1 when a monitor cannot be reached by
	// ffmpeg's D3D11 adapter. ddagrab cannot address it, so it has to use GDI
	// instead of failing to build the input.
	if got := monWithoutOutput.ChooseGrabber(withDDA); got != GrabberGDigrab {
		t.Errorf("monitor with no DXGI output: %q, want %q", got, GrabberGDigrab)
	}
	// A window is cropped out of a duplicated output, which is both cheaper
	// and the only way to see a GPU-composited window.
	if got := onScreen.ChooseGrabber(withDDA); got != GrabberDDAGrab {
		t.Errorf("window on a known monitor with ddagrab: %q, want %q", got, GrabberDDAGrab)
	}
	if got := onScreen.ChooseGrabber(withoutDDA); got != GrabberGDigrab {
		t.Errorf("window without ddagrab: %q, want %q", got, GrabberGDigrab)
	}
	if got := offScreen.ChooseGrabber(withDDA); got != GrabberGDigrab {
		t.Errorf("window with no known monitor: %q, want %q", got, GrabberGDigrab)
	}
}

func TestChooseGrabberPerOutputDDAGrab(t *testing.T) {
	mon0 := Target{Kind: KindMonitor, Monitor: Monitor{Index: 0, DXGIIndex: 0}}
	mon1 := Target{Kind: KindMonitor, Monitor: Monitor{Index: 1, DXGIIndex: 1}}
	monBad := Target{Kind: KindMonitor, Monitor: Monitor{Index: 2, DXGIIndex: -1}}

	// Sentinel -1: DDAGrabHealthy means global ddagrab available.
	// Explicit DDAGrabUnhealthy for output 1 means that specific output failed the luma probe.
	caps := Capabilities{DDAGrab: map[int]int8{-1: DDAGrabHealthy, 1: DDAGrabUnhealthy}}

	if got := mon0.ChooseGrabber(caps); got != GrabberDDAGrab {
		t.Errorf("monitor 0 (output_idx=0 healthy): %q, want %q", got, GrabberDDAGrab)
	}
	if got := mon1.ChooseGrabber(caps); got != GrabberGDigrab {
		t.Errorf("monitor 1 (output_idx=1 black): %q, want %q", got, GrabberGDigrab)
	}
	if got := monBad.ChooseGrabber(caps); got != GrabberGDigrab {
		t.Errorf("monitor with DXGIIndex=-1: %q, want %q", got, GrabberGDigrab)
	}

	// Empty map = no ddagrab at all (global probe failed)
	if got := mon0.ChooseGrabber(Capabilities{DDAGrab: map[int]int8{}}); got != GrabberGDigrab {
		t.Errorf("empty capabilities map: %q, want %q", got, GrabberGDigrab)
	}

	// nil map = no ddagrab probed yet, safe fallback
	if got := mon0.ChooseGrabber(Capabilities{DDAGrab: nil}); got != GrabberGDigrab {
		t.Errorf("nil capabilities map: %q, want %q", got, GrabberGDigrab)
	}

	// Sentinel -1: healthy, no per-output entries = all outputs healthy
	allHealthy := Capabilities{DDAGrab: map[int]int8{-1: DDAGrabHealthy}}
	if got := mon0.ChooseGrabber(allHealthy); got != GrabberDDAGrab {
		t.Errorf("all healthy monitor 0: %q, want %q", got, GrabberDDAGrab)
	}
	if got := mon1.ChooseGrabber(allHealthy); got != GrabberDDAGrab {
		t.Errorf("all healthy monitor 1: %q, want %q", got, GrabberDDAGrab)
	}

	// Sentinel -1: unhealthy = no ddagrab at all
	allUnhealthy := Capabilities{DDAGrab: map[int]int8{-1: DDAGrabUnhealthy}}
	if got := mon0.ChooseGrabber(allUnhealthy); got != GrabberGDigrab {
		t.Errorf("all unhealthy: %q, want %q", got, GrabberGDigrab)
	}
}

func TestInputSpecDDAGrabWindowCrops(t *testing.T) {
	target := Target{
		Kind:    KindWindow,
		Window:  Window{Rect: Rect{X: 2020, Y: 40, Width: 1200, Height: 900}},
		Monitor: Monitor{DXGIIndex: 1, Bounds: Rect{X: 1920, Width: 2560, Height: 1440}},
	}

	spec, err := target.InputSpec(InputOptions{Grabber: GrabberDDAGrab, FPS: 60})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}
	if !contains(join(spec.Args), "ddagrab=output_idx=1:framerate=60") {
		t.Errorf("args %v do not duplicate the window's monitor", spec.Args)
	}
	// The window starts 100 pixels into the monitor, and H.264 needs an even
	// frame size.
	for _, want := range []string{"hwdownload", "crop=1200:900:100:40", "scale=trunc(iw/2)*2"} {
		if !contains(spec.Filters, want) {
			t.Errorf("filters %q do not contain %q", spec.Filters, want)
		}
	}
}

func TestInputSpecDDAGrab(t *testing.T) {
	target := Target{
		Kind:    KindMonitor,
		Monitor: Monitor{Index: 1, DXGIIndex: 4, Bounds: Rect{Width: 2560, Height: 1440}, Rotation: 90},
	}

	spec, err := target.InputSpec(InputOptions{Grabber: GrabberDDAGrab, FPS: 60, DrawMouse: true, Scale: "1280x720"})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}

	if !contains(join(spec.Args), "ddagrab=output_idx=4:framerate=60:draw_mouse=1") {
		t.Errorf("args %v do not select dxgi output 4 at 60fps with the cursor", spec.Args)
	}
	if !contains(join(spec.Args), "d3d11va") {
		t.Errorf("args %v do not create the D3D11 device that hwdownload needs", spec.Args)
	}
	// A rotated monitor must be transposed back, the frames downloaded, and the
	// scale applied, in that order.
	for _, want := range []string{"hwdownload", "format=bgra", "transpose=1", "scale=1280x720"} {
		if !contains(spec.Filters, want) {
			t.Errorf("filters %q do not contain %q", spec.Filters, want)
		}
	}
	if index(spec.Filters, "hwdownload") > index(spec.Filters, "transpose") {
		t.Errorf("filters %q transpose before downloading, which is impossible", spec.Filters)
	}
}

func TestInputSpecGDigrabMonitor(t *testing.T) {
	target := Target{
		Kind:    KindMonitor,
		Monitor: Monitor{Index: 1, Bounds: Rect{X: -1920, Y: 293, Width: 1920, Height: 1080}},
	}

	spec, err := target.InputSpec(InputOptions{Grabber: GrabberGDigrab, FPS: 30, DrawMouse: false})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}

	args := join(spec.Args)
	for _, want := range []string{"-f gdigrab", "-offset_x -1920", "-offset_y 293", "-video_size 1920x1080", "-i desktop", "-draw_mouse 0"} {
		if !contains(args, want) {
			t.Errorf("args %q do not contain %q", args, want)
		}
	}
	// GDI already sees the desktop the way the user does, so there is no
	// transpose even on a rotated monitor.
	if contains(spec.Filters, "transpose") {
		t.Errorf("filters %q should not transpose a GDI capture", spec.Filters)
	}
}

func TestInputSpecGDigrabWindow(t *testing.T) {
	target := Target{
		Kind:    KindWindow,
		Window:  Window{Title: `Notes: C:\tmp`, Class: "Notepad", Rect: Rect{X: -500, Y: 100, Width: 1000, Height: 700}},
		Monitor: Monitor{Bounds: Rect{X: -1920, Width: 2560, Height: 1080}},
	}

	spec, err := target.InputSpec(InputOptions{Grabber: GrabberGDigrab, FPS: 30, DrawMouse: true})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}

	// The window is clipped to its monitor so the offsets stay inside the
	// desktop, which is what keeps GDI from returning black.
	args := join(spec.Args)
	for _, want := range []string{"-offset_x -500", "-offset_y 100", "-video_size 1000x700", "-i desktop", "-draw_mouse 1"} {
		if !contains(args, want) {
			t.Errorf("args %q do not contain %q", args, want)
		}
	}
	if contains(args, "title=") {
		t.Errorf("args %q still select the window by title, which is black for GPU-composited windows", args)
	}
	if !contains(spec.Filters, "scale=trunc(iw/2)*2") {
		t.Errorf("filters %q do not round the window size to even dimensions", spec.Filters)
	}
}

func TestInputSpecDDAGrabClipsWindowToMonitor(t *testing.T) {
	target := Target{
		Kind:    KindWindow,
		Window:  Window{Rect: Rect{X: 1600, Y: 0, Width: 800, Height: 600}},
		Monitor: Monitor{DXGIIndex: 0, Bounds: Rect{Width: 1920, Height: 1080}},
	}
	spec, err := target.InputSpec(InputOptions{Grabber: GrabberDDAGrab})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}
	// Only the 320 pixels that are still on screen can be captured.
	if !contains(spec.Filters, "crop=320:600:1600:0") {
		t.Errorf("filters %q do not clip the window to its monitor", spec.Filters)
	}
}

func TestInputSpecWindowWithoutMonitor(t *testing.T) {
	// ddagrab needs to know which monitor to duplicate, so a window whose
	// monitor is unknown is rejected there...
	target := Target{
		Kind:   KindWindow,
		Window: Window{Rect: Rect{X: 100, Y: 50, Width: 800, Height: 600}},
	}
	if _, err := target.InputSpec(InputOptions{Grabber: GrabberDDAGrab}); err == nil {
		t.Fatal("ddagrab accepted a window with no monitor")
	}
	// ...but GDI addresses the virtual desktop by rectangle, so it can still
	// capture it without one.
	spec, err := target.InputSpec(InputOptions{Grabber: GrabberGDigrab})
	if err != nil {
		t.Fatalf("gdigrab rejected a window with no monitor: %v", err)
	}
	if !contains(join(spec.Args), "-video_size 800x600") {
		t.Errorf("args %v do not use the window rectangle", spec.Args)
	}
}

func TestInputSpecUnknownGrabber(t *testing.T) {
	if _, err := (Target{Kind: KindMonitor}).InputSpec(InputOptions{Grabber: "magic"}); err == nil {
		t.Fatal("an unknown grabber was accepted")
	}
}

func TestInputSpecDefaultsFPS(t *testing.T) {
	target := Target{Kind: KindMonitor, Monitor: Monitor{Bounds: Rect{Width: 100, Height: 100}}}
	spec, err := target.InputSpec(InputOptions{Grabber: GrabberGDigrab})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}
	if !contains(join(spec.Args), "-framerate 30") {
		t.Errorf("args %q do not contain a default framerate", spec.Args)
	}
}

func TestScaleFiltersKeepsDimensionsEven(t *testing.T) {
	// H.264 with 4:2:0 chroma needs even dimensions, and users type odd ones.
	got := scaleFilters("1921x1081")
	if len(got) != 2 {
		t.Fatalf("scaleFilters = %v, want a scale plus an even-dimension correction", got)
	}
	if !contains(got[1], "trunc(iw/2)*2") {
		t.Errorf("second filter = %q, want the trunc correction", got[1])
	}
	if len(scaleFilters("")) != 0 {
		t.Error("an empty scale should add no filters at all")
	}
}

func TestRotationFilters(t *testing.T) {
	cases := []struct {
		degrees int
		want    []string
	}{
		{0, nil},
		{90, []string{"transpose=1"}},
		{180, []string{"transpose=1", "transpose=1"}},
		{270, []string{"transpose=2"}},
		{45, nil}, // not a real rotation, must not blow up
	}

	for _, c := range cases {
		got := rotationFilters(c.degrees)
		if len(got) != len(c.want) {
			t.Errorf("rotationFilters(%d) = %v, want %v", c.degrees, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("rotationFilters(%d) = %v, want %v", c.degrees, got, c.want)
				break
			}
		}
	}
}

func TestEscapeFFmpegValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a:b", `a\:b`},
		{`C:\tmp`, `C\:\\tmp`},
		{"a,b", `a\,b`},
		{"100%", `100\%`},
	}
	for _, c := range cases {
		if got := escapeFFmpegValue(c.in); got != c.want {
			t.Errorf("escapeFFmpegValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTargetDescribe(t *testing.T) {
	mon := Target{Kind: KindMonitor, Monitor: Monitor{Index: 2, DXGIIndex: 3, Bounds: Rect{Width: 1920, Height: 1080}}}
	if got := mon.Describe(); !contains(got, "monitor 2") || !contains(got, "dxgi output 3") {
		t.Errorf("monitor Describe = %q", got)
	}

	win := Target{Kind: KindWindow, Window: Window{
		Title: "Quarterly layoffs - Mail", Class: "Notepad", PID: 4321,
		Rect: Rect{Width: 800, Height: 600},
	}}
	got := win.Describe()
	if !contains(got, "Notepad") || !contains(got, "4321") {
		t.Errorf("window Describe = %q, want the class and the pid", got)
	}
	if contains(got, "layoffs") {
		t.Errorf("window Describe = %q, which leaks the window title into the log", got)
	}
}

// Nothing the user might read later may contain a window title. Describe and
// Reason end up in streamy.log, in a tray notification and in bug reports, so
// this is checked for both ways a window can be chosen.
func TestDiagnosticsNeverContainAWindowTitle(t *testing.T) {
	const secret = "divorce-papers-DRAFT-final-v2.docx"

	cfgFor := func(defaultTarget string) config.Config {
		cfg := config.Default()
		cfg.Capture.DefaultTarget = defaultTarget
		return cfg
	}

	t.Run("a fullscreen window", func(t *testing.T) {
		full := Rect{Width: 1920, Height: 1080}
		d := Desktop{
			Monitors:   []Monitor{{Index: 0, Bounds: full, Primary: true, DXGIIndex: 0}},
			Foreground: window(secret, "Notepad", 4321, full),
			SelfPID:    1,
		}

		target, err := Decide(d, cfgFor(config.TargetPrimary))
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if contains(target.Reason, secret) {
			t.Errorf("Reason leaks the title: %q", target.Reason)
		}
		if contains(target.Describe(), secret) {
			t.Errorf("Describe leaks the title: %q", target.Describe())
		}
		if !contains(target.Reason, "Notepad") {
			t.Errorf("Reason = %q, want it to identify the window by class", target.Reason)
		}
	})

	t.Run("the focused window", func(t *testing.T) {
		// Deliberately not fullscreen, so the fallback target is the one that
		// picks it up.
		d := desktop()
		d.Foreground = window(secret, "Notepad", 4321, Rect{X: 100, Y: 100, Width: 800, Height: 600})

		target, err := Decide(d, cfgFor(config.TargetFocused))
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if target.Kind != KindWindow {
			t.Fatalf("Kind = %v, want window", target.Kind)
		}
		// The target must still keep the title: gdigrab needs it to find the
		// window. It is only the human-readable strings that must not have it.
		if target.Window.Title != secret {
			t.Error("the target lost the title, so gdigrab cannot find the window")
		}
		if contains(target.Reason, secret) {
			t.Errorf("Reason leaks the title: %q", target.Reason)
		}
		if contains(target.Describe(), secret) {
			t.Errorf("Describe leaks the title: %q", target.Describe())
		}
		if !contains(target.Describe(), "4321") {
			t.Errorf("Describe = %q, want it to name the owning process", target.Describe())
		}
	})
}

// containsStrings is here so the assertions above read like sentences.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func index(haystack, needle string) int { return strings.Index(haystack, needle) }

func join(parts []string) string { return strings.Join(parts, " ") }

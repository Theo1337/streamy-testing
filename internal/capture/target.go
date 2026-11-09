// Package capture answers two questions: what should we record right now, and
// how do we tell ffmpeg to record it?
//
// The package is deliberately split in two halves:
//
//   - Platform code (win_display.go, win_window.go, ...) produces a Desktop
//     snapshot: a plain list of monitors and windows, with no opinions.
//   - Decide, in this file, turns that snapshot into a Target using the rules
//     from INSTRUCTIONS.md. It is pure data in, data out, so the rules can be
//     tested without a desktop, a game, or Windows at all.
//
// That split is also what makes macOS and Linux support a matter of writing a
// new snapshot producer rather than a new set of rules.
package capture

import (
	"errors"
	"fmt"
	"strings"

	"streamy/internal/config"
)

// Kind is the sort of thing being recorded.
type Kind uint8

const (
	// KindMonitor records a whole display.
	KindMonitor Kind = iota
	// KindWindow records a single window.
	KindWindow
)

func (k Kind) String() string {
	if k == KindWindow {
		return "window"
	}
	return "monitor"
}

// DisplayPath describes one entry from the Windows display configuration API.
//
// It is exported for diagnostics: the adapter LUID, the driver's own target
// identifier, the desktop rectangle and the rotation are visible together
// nowhere else, and a capture that lands on the wrong screen is usually
// explained by this data.
type DisplayPath struct {
	// AdapterLUID identifies the graphics adapter that drives this output.
	AdapterLUID uint64
	// TargetID is the driver's identifier for the output. It is not the
	// zero-based index DXGI (and therefore ffmpeg) uses.
	TargetID uint32
	// Desktop is the rectangle this output occupies in virtual desktop
	// coordinates.
	//
	// It is reported in the orientation the display is configured in, except
	// that for a rotated output the *size* is the unrotated surface: the
	// portrait monitor on the machine this was written on is 1080x1920 on the
	// desktop but appears here as 1920x1080 at 1920,-232, with Rotation 90.
	// That quirk is why the app does not join monitors and outputs on this
	// rectangle; DXGI's HMONITOR does the job exactly.
	Desktop Rect
	// ActiveSize is the resolution on the wire, before rotation is applied.
	ActiveSize Rect
	// Rotation in degrees: 0, 90, 180 or 270.
	Rotation int
	// OutputTechnology is a DISPLAYCONFIG_VIDEO_OUTPUT_TECHNOLOGY value.
	OutputTechnology uint32
}

// Rect is a rectangle in virtual desktop coordinates, in physical pixels. It
// can have negative X or Y: a monitor left of the primary one starts at a
// negative X.
type Rect struct{ X, Y, Width, Height int }

// Empty reports whether the rectangle has no area.
func (r Rect) Empty() bool { return r.Width <= 0 || r.Height <= 0 }

// String renders the rectangle in the familiar ffmpeg style, `1920x1080+0+0`.
func (r Rect) String() string {
	return fmt.Sprintf("%dx%d+%d+%d", r.Width, r.Height, r.X, r.Y)
}

// CoveredBy reports whether other covers all of r, allowing up to tolerance
// pixels of slack on every edge.
//
// Written as `monitor.Bounds.CoveredBy(window.Rect)` so it reads as the
// question being asked: is this monitor fully covered by that window? It is not
// an equality test in practice: borderless windows are often a pixel or two
// off, and some games report a frame slightly larger than the monitor.
func (r Rect) CoveredBy(other Rect, tolerance int) bool {
	return other.X <= r.X+tolerance &&
		other.Y <= r.Y+tolerance &&
		other.X+other.Width >= r.X+r.Width-tolerance &&
		other.Y+other.Height >= r.Y+r.Height-tolerance
}

// Intersect returns the part of r that is inside other, or an empty rectangle
// when they do not overlap. A window routinely sticks out past its monitor, and
// the part that is off screen cannot be captured.
func (r Rect) Intersect(other Rect) Rect {
	x := max(r.X, other.X)
	y := max(r.Y, other.Y)
	right := min(r.X+r.Width, other.X+other.Width)
	bottom := min(r.Y+r.Height, other.Y+other.Height)
	if right <= x || bottom <= y {
		return Rect{}
	}
	return Rect{X: x, Y: y, Width: right - x, Height: bottom - y}
}

// FullscreenTolerance is how many pixels a window may be smaller than its
// monitor and still count as fullscreen.
//
// Eight pixels is comfortably larger than the slop real borderless windows
// report, and comfortably smaller than a taskbar (about 40-60 pixels), which
// is what keeps a simply maximised window from being mistaken for a fullscreen
// one on a monitor that has a taskbar.
const FullscreenTolerance = 8

// Monitor is one display.
type Monitor struct {
	// Index is this monitor's position in the desktop's list.
	Index int
	// DXGIIndex is the index ffmpeg's ddagrab filter expects for this monitor.
	// It is not the same as Index: Index is a position in Windows' list of
	// views, DXGIIndex is a position in the graphics adapter's list of outputs.
	// It is -1 when the monitor could not be matched to an output, in which
	// case capture falls back to GDI. See win_dxgi.go for the join.
	DXGIIndex int
	// Bounds is the monitor rectangle in virtual desktop coordinates, in the
	// orientation the user sees.
	Bounds Rect
	// SignalSize is the size the display renders at before rotation. For the
	// portrait monitor used while developing this, Bounds is 1080x1920 while
	// Desktop Duplication hands out a 1920x1080 image.
	SignalSize Rect
	// WorkArea is Bounds minus the taskbar.
	WorkArea Rect
	// Primary is true for the monitor at the desktop origin.
	Primary bool
	// Rotation is the display rotation in degrees: 0, 90, 180 or 270.
	Rotation int
	// Name is the GDI device name, for example `\\.\DISPLAY1`.
	Name string
}

// Window is a top level window.
type Window struct {
	// Handle is the Win32 HWND, as an integer so this type stays usable on
	// other platforms.
	Handle uintptr
	Title  string
	Class  string
	// PID of the process that owns the window.
	PID uint32
	// Rect is the visible frame of the window. On Windows this is the DWM frame
	// bounds, which excludes the invisible borders around a maximised window.
	Rect Rect
	// RawRect is the raw GetWindowRect bounds of the window, including any
	// invisible borders that DWM hides from the user. PrintWindow renders into
	// a DIB sized to these dimensions; capture code uses RawRect to compute
	// correct offsets within the PrintWindow output.
	// On non-Windows builds (or when GetWindowRect fails) this equals Rect.
	RawRect Rect
	// ClientRect is the client area of the window in screen coordinates,
	// excluding the title bar and window borders (DWM frame).
	ClientRect Rect
	// Minimized and Visible come straight from the window state.
	Minimized bool
	Visible   bool
}

// Desktop is a snapshot of everything on screen right now.
type Desktop struct {
	Monitors     []Monitor
	Foreground   *Window
	MouseMonitor int // index into Monitors, or -1
	// SelfPID is this process, whose windows must never be captured.
	SelfPID uint32
}

// Target is the resolved thing to record.
type Target struct {
	Kind Kind
	// Monitor is set when Kind is KindMonitor, and also for a window target
	// whose monitor we know (useful for logging and for the picker).
	Monitor Monitor
	// Window is set when Kind is KindWindow.
	Window Window
	// Fullscreen is true when the target was chosen because a window covers
	// its whole monitor. It is what makes Desktop Duplication the right
	// grabber.
	Fullscreen bool
	// Cursor draws the mouse pointer into the recording.
	Cursor bool
	// Reason explains the choice in one line. It is logged, and it is the first
	// thing to look at when the wrong screen got recorded.
	Reason string
}

// Describe returns a short human readable description of the target.
//
// A window is named by its class and its owning process, never by its title. A
// title is whatever the user happens to have open at that moment — a message, a
// half-written document, a page — and this string is written to the log, put in
// a notification and pasted into bug reports. Titles are used to *find* a
// window (the CLI's -window and the picker) and for nothing else. `-titles` is
// the single switch that lets one appear in output.
func (t Target) Describe() string {
	if t.Kind == KindWindow {
		return fmt.Sprintf("window class %q pid %d (%s)", t.Window.Class, t.Window.PID, t.Window.Rect)
	}
	return fmt.Sprintf("monitor %d (%s, dxgi output %d)", t.Monitor.Index, t.Monitor.Bounds, t.Monitor.DXGIIndex)
}

// windowID names a window for a log line without naming its title, for the
// reason given on Describe.
func windowID(w Window) string {
	return fmt.Sprintf("class %q pid %d", w.Class, w.PID)
}

// ignoredWindowClasses are shell windows that must never become the target.
//
// Progman and WorkerW are the desktop itself: capturing them records the
// wallpaper, the icons and nothing else. The tray windows are the taskbar.
var ignoredWindowClasses = map[string]bool{
	"Progman":                true,
	"WorkerW":                true,
	"Shell_TrayWnd":          true,
	"Shell_SecondaryTrayWnd": true,
}

// Decide applies the automatic target selection rules:
//
//  1. If the foreground window is a real window and covers its whole monitor,
//     AND that monitor can be captured with Desktop Duplication (DXGIIndex >= 0),
//     capture that monitor. Exclusive fullscreen games and borderless-windowed
//     games both land here, and per-window capture of a fullscreen game is
//     exactly the case that shows up black.
//  2. Otherwise fall back to the configured default target, preferring a monitor
//     that can be captured with Desktop Duplication (DXGIIndex >= 0) to avoid
//     black screens on multi-GPU systems.
//
// The returned Target always carries a Reason, including when the fallback was
// used, so a misdetection can be diagnosed from the log alone.
func Decide(d Desktop, cfg config.Config) (Target, error) {
	if len(d.Monitors) == 0 {
		return Target{}, errors.New("no monitors found")
	}

	note := ""
	if cfg.Capture.AlwaysShowPicker {
		// capture.alwaysShowPicker exists in the settings from phase 1 so that
		// the picker window can honour it, but the picker itself arrives in
		// phase 6.
		note = ` (capture.alwaysShowPicker is set, but the picker arrives in phase 6, so auto-selecting)`
	}

	// 1. A fullscreen foreground window wins, but only if its monitor can be
	// captured with Desktop Duplication. If the monitor has no DXGI output
	// (DXGIIndex < 0), Desktop Duplication is unavailable and capture would
	// fall back to GDI, which produces black for fullscreen games.
	if fg := d.Foreground; fg != nil && capturable(*fg, d.SelfPID) {
		if idx := d.monitorContaining(fg.Rect); idx >= 0 {
			m := d.Monitors[idx]
			if m.Bounds.CoveredBy(fg.Rect, FullscreenTolerance) {
				if m.DXGIIndex >= 0 {
					return Target{
						Kind:       KindMonitor,
						Monitor:    m,
						Fullscreen: true,
						Cursor:     true, // cursor is always shown since the setting was removed
						Reason: fmt.Sprintf("foreground window (%s) fills monitor %d (%s), capturing the whole monitor so fullscreen games do not come out black%s",
							windowID(*fg), m.Index, m.Bounds, note),
					}, nil
				}
				// Monitor has no DXGI output (likely on a different GPU), so
				// Desktop Duplication would fail and GDI would produce black.
				// Fall through to default target selection instead.
				note += fmt.Sprintf("; ignoring fullscreen window on monitor %d (no DXGI output, would capture black)", m.Index)
			}
		}
	}

	// 2. Fall back to the configured default, preferring a monitor with
	// valid DXGIIndex to avoid black screens on multi-GPU systems.
	switch cfg.Capture.DefaultTarget {
	case config.TargetFocused:
		if fg := d.Foreground; fg != nil && capturable(*fg, d.SelfPID) {
			return windowTarget(*fg, d, cfg, fmt.Sprintf(
				"capture.defaultTarget is %q, using the focused window (%s)%s",
				config.TargetFocused, windowID(*fg), note)), nil
		}
		return monitorTarget(d.primaryWithDDAGrab(), cfg, "capture.defaultTarget is \"focused\" but nothing capturable has focus, using a monitor with Desktop Duplication"+note), nil

	case config.TargetMonitor:
		idx := cfg.Capture.MonitorIndex
		if idx >= 0 && idx < len(d.Monitors) {
			m := d.Monitors[idx]
			if m.DXGIIndex >= 0 {
				return monitorTarget(m, cfg, fmt.Sprintf(
					"capture.defaultTarget is \"monitor\", using capture.monitorIndex %d%s", idx, note)), nil
			}
			// Requested monitor has no DXGI output, try to find one that does.
			if alt := d.monitorWithDDAGrab(); alt != nil {
				return monitorTarget(*alt, cfg, fmt.Sprintf(
					"capture.defaultTarget is \"monitor\" but monitor %d has no DXGI output, using monitor %d with Desktop Duplication%s",
					idx, alt.Index, note)), nil
			}
			return monitorTarget(m, cfg, fmt.Sprintf(
				"capture.defaultTarget is \"monitor\", using capture.monitorIndex %d (no DXGI output, will fall back to GDI)%s",
				idx, note)), nil
		}
		return monitorTarget(d.primaryWithDDAGrab(), cfg, fmt.Sprintf(
			"capture.defaultTarget is \"monitor\" but capture.monitorIndex %d does not exist (%d monitors), using a monitor with Desktop Duplication%s",
			idx, len(d.Monitors), note)), nil

	default: // config.TargetPrimary, and anything unexpected
		return monitorTarget(d.primaryWithDDAGrab(), cfg, "capture.defaultTarget is \"primary\" (and no fullscreen window was found)"+note), nil
	}
}

// capturable reports whether a window is a legitimate capture target.
func capturable(w Window, selfPID uint32) bool {
	switch {
	case !w.Visible:
		return false
	case w.Minimized:
		return false
	case strings.TrimSpace(w.Title) == "":
		// Windows without a title are almost always invisible helper windows
		// (IME, drag-and-drop shims, tooltips).
		return false
	case w.Rect.Empty():
		return false
	case selfPID != 0 && w.PID == selfPID:
		// Our own tray window and notifications must never be the target.
		return false
	case ignoredWindowClasses[w.Class]:
		return false
	default:
		return true
	}
}

func monitorTarget(m Monitor, cfg config.Config, reason string) Target {
	return Target{
		Kind:    KindMonitor,
		Monitor: m,
		Cursor:  true, // cursor is always shown since the setting was removed
		Reason:  reason,
	}
}

func windowTarget(w Window, d Desktop, cfg config.Config, reason string) Target {
	t := Target{
		Kind:   KindWindow,
		Window: w,
		Cursor: true, // cursor is always shown since the setting was removed
		Reason: reason,
	}
	if idx := d.monitorContaining(w.Rect); idx >= 0 {
		t.Monitor = d.Monitors[idx]
	}
	return t
}

// WithDesktop fills in the monitor a window target sits on, when it is not set
// already.
//
// Capture reads the composited desktop, so a window is recorded as the
// rectangle it occupies. Knowing which monitor that rectangle belongs to lets
// ffmpeg duplicate that monitor and crop the window out of it, which is much
// cheaper than a GDI copy. Targets created by the CLI or the picker do not
// always carry a monitor, so this is applied before capture starts.
func (t Target) WithDesktop(d Desktop) Target {
	if t.Kind != KindWindow || !t.Monitor.Bounds.Empty() {
		return t
	}
	if idx := d.monitorContaining(t.Window.Rect); idx >= 0 {
		t.Monitor = d.Monitors[idx]
	}
	return t
}

// windowRegion is the visible part of a window's rectangle, in desktop
// coordinates.
//
// Capture reads the composited desktop, so a window is grabbed as the rectangle
// it occupies rather than through its own drawing surface. Clipping to the
// monitor keeps the rectangle inside the duplicated (or GDI) surface, and it
// also means a window dragged halfway off screen still captures what is left.
//
// For window capture, we use the client rect (excluding title bar and borders)
// instead of the frame rect to avoid capturing the window frame.
func (t Target) windowRegion() (Rect, bool) {
	if t.Kind != KindWindow {
		return Rect{}, false
	}
	// Use ClientRect if available, otherwise fall back to frame Rect
	winRect := t.Window.ClientRect
	if winRect.Empty() {
		winRect = t.Window.Rect
	}
	region := winRect.Intersect(t.Monitor.Bounds)
	if region.Empty() {
		return Rect{}, false
	}
	return region, true
}

// monitorContaining returns the index of the monitor with the largest overlap
// with r, or -1 when r is off screen entirely.
//
// Largest overlap rather than "contains the point" because a window is
// routinely a little larger than the monitor it sits on.
func (d Desktop) monitorContaining(r Rect) int {
	best, bestArea := -1, 0
	for i, m := range d.Monitors {
		area := intersectionArea(m.Bounds, r)
		if area > bestArea {
			best, bestArea = i, area
		}
	}
	return best
}

func (d Desktop) primary() Monitor {
	for _, m := range d.Monitors {
		if m.Primary {
			return m
		}
	}
	return d.Monitors[0]
}

func intersectionArea(a, b Rect) int {
	w := min(a.X+a.Width, b.X+b.Width) - max(a.X, b.X)
	h := min(a.Y+a.Height, b.Y+b.Height) - max(a.Y, b.Y)
	if w <= 0 || h <= 0 {
		return 0
	}
	return w * h
}

// primaryWithDDAGrab returns the primary monitor if it has a valid DXGIIndex,
// otherwise returns the first monitor with a valid DXGIIndex, or the primary
// monitor as a last resort. This avoids black screens on multi-GPU systems
// where the primary monitor is on a different GPU than the capture adapter.
func (d Desktop) primaryWithDDAGrab() Monitor {
	// First, try the actual primary monitor
	for _, m := range d.Monitors {
		if m.Primary && m.DXGIIndex >= 0 {
			return m
		}
	}
	// Primary monitor has no DXGI output (different GPU), find any monitor with DDAGrab
	for _, m := range d.Monitors {
		if m.DXGIIndex >= 0 {
			return m
		}
	}
	// No monitor has DDAGrab, fall back to the primary monitor (will use GDI)
	return d.primary()
}

// monitorWithDDAGrab returns the first monitor with a valid DXGIIndex, or nil
// if none exist.
func (d Desktop) monitorWithDDAGrab() *Monitor {
	for i := range d.Monitors {
		if d.Monitors[i].DXGIIndex >= 0 {
			return &d.Monitors[i]
		}
	}
	return nil
}

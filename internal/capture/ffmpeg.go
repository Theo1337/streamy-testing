package capture

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Grabber is the ffmpeg input method used to read pixels off the screen.
type Grabber string

const (
	// GrabberDDAGrab uses the Desktop Duplication API through ffmpeg's ddagrab
	// filter. It hands out D3D11 textures straight from the compositor, which
	// is the only reliable way to capture an exclusive fullscreen game:
	// copying those frames with GDI tends to produce a black image.
	GrabberDDAGrab Grabber = "ddagrab"

	// GrabberGDigrab uses the GDI BitBlt path. It is slower, it cannot always
	// see fullscreen games, and it is the fallback. Windows are captured as the
	// rectangle they occupy on the desktop, because grabbing a window by title
	// hands back a black picture for every window Windows composites on the
	// GPU (Chrome, Electron, most modern games).
	GrabberGDigrab Grabber = "gdigrab"

	// GrabberWindow is not an ffmpeg input at all: Go calls PrintWindow and
	// feeds the resulting BGRA frames to ffmpeg over a loopback socket. It is
	// the only mode that shows the streamed window's own pixels when another
	// window is covering it, even for GPU-composited windows.
	GrabberWindow Grabber = "window"
)

// Capabilities describes what the local ffmpeg can actually do. It is measured
// once at startup (see internal/encode) rather than assumed from the filter
// list, because "ddagrab is present" and "ddagrab works on this machine" are
// different statements.
type Capabilities struct {
	// DDAGrab maps each DXGI output index to whether ddagrab works for that
	// output. Values: 0 = unknown (not probed or use sentinel), 1 = healthy,
	// 2 = unhealthy (black frames detected, fall back to GDI).
	// Using int8 instead of bool works around a Go 1.27.0 bug where
	// map[int]bool with false values returns ok=false on two-value lookup.
	// This per-output granularity handles Optimus laptops where the D3D11
	// device is created on one GPU but only some monitors are driven by it.
	DDAGrab map[int]int8
}

const (
	DDAGrabUnknown int8 = 0 // not probed / use sentinel
	DDAGrabHealthy int8 = 1 // probe passed, ddagrab works
	DDAGrabUnhealthy int8 = 2 // probe failed, black frames detected
)

// ChooseGrabber picks the grabber to use for this target.
func (t Target) ChooseGrabber(caps Capabilities) Grabber {
	if caps.DDAGrab == nil {
		return GrabberGDigrab
	}
	// Sentinel key -1 means "all outputs healthy" (used by the CLI tool
	// and as a fallback when per-output probing couldn't run).
	if v, ok := caps.DDAGrab[-1]; ok && v == DDAGrabHealthy {
		// Global ddagrab is available; continue to per-output checks below.
	} else {
		// Empty map, missing sentinel, or sentinel not healthy = no ddagrab at all.
		return GrabberGDigrab
	}
	// ddagrab can only address an output that was matched to ffmpeg's D3D11
	// adapter. DXGIIndex is -1 when the monitor is driven by another graphics
	// adapter, is detached, or could not be matched at all; pointing ddagrab
	// at it would fail to build the input, so fall back to GDI for the whole
	// monitor (including a window on it).
	// This check must come BEFORE the window logic so that a monitor with
	// DXGIIndex=-1 never incorrectly selects DDAGrab.
	if t.Monitor.DXGIIndex < 0 {
		return GrabberGDigrab
	}
	// Per-output check: if this specific output failed the luma probe (explicit unhealthy),
	// fall back to GDI for just this monitor. If the key is missing or unknown,
	// the sentinel -1: healthy means it's healthy.
	if v, ok := caps.DDAGrab[t.Monitor.DXGIIndex]; ok && v == DDAGrabUnhealthy {
		return GrabberGDigrab
	}
	if t.Kind == KindWindow {
		// ddagrab duplicates a whole output and has no window mode, but the
		// window's rectangle can be cropped out of that surface. Doing so is
		// both cheaper than GDI and, unlike grabbing a window by title, it
		// sees windows that are composited on the GPU.
		if _, ok := t.windowRegion(); ok && t.Monitor.Rotation == 0 {
			return GrabberDDAGrab
		}
		return GrabberGDigrab
	}
	return GrabberDDAGrab
}

// InputOptions tunes how the capture source is opened.
type InputOptions struct {
	Grabber   Grabber
	FPS       int
	DrawMouse bool
	// Scale resizes the frames before they reach the encoder, for example
	// "1280x720". Doing it here rather than with encoder specific options keeps
	// the encoder arguments identical for every encoder.
	Scale string
}

// InputSpec is the ffmpeg side of a Target: the arguments that open the source,
// plus the video filter chain that prepares the frames.
type InputSpec struct {
	// Args are the input arguments, e.g. `-f gdigrab -i desktop`.
	Args []string
	// Filters is the video filter chain without the -vf flag. Empty when the
	// frames need no work at all.
	Filters string
	// Grabber records which grabber was used, for logging.
	Grabber Grabber
}

// InputSpec builds the ffmpeg arguments that open this target.
func (t Target) InputSpec(opts InputOptions) (InputSpec, error) {
	if opts.FPS <= 0 {
		opts.FPS = 30
	}

	switch opts.Grabber {
	case GrabberDDAGrab:
		return t.ddagrabSpec(opts)
	case GrabberGDigrab:
		return t.gdigrabSpec(opts)
	case "":
		return InputSpec{}, errors.New("no grabber chosen")
	default:
		return InputSpec{}, fmt.Errorf("unknown grabber %q", opts.Grabber)
	}
}

// ddagrabSpec builds the Desktop Duplication input.
//
// ddagrab is a filter rather than a capture device, so it is attached to a
// lavfi source. It also needs a D3D11 device to hand its textures to, hence the
// `-init_hw_device d3d11va`, which is what makes `hwdownload` work afterwards.
//
// A window target is captured by duplicating the monitor under it and cropping
// the window's rectangle out of the frame. Desktop Duplication hands out the
// composited desktop, so this sees exactly what GDI's per-window BitBlt fails
// to see: GPU-rendered windows, which otherwise come out black.
func (t Target) ddagrabSpec(opts InputOptions) (InputSpec, error) {
	if t.Kind != KindMonitor && t.Kind != KindWindow {
		return InputSpec{}, fmt.Errorf("cannot capture a %s with ddagrab", t.Kind)
	}
	if t.Monitor.DXGIIndex < 0 {
		return InputSpec{}, errors.New("monitor has no known DXGI output")
	}

	args := []string{
		"-init_hw_device", "d3d11va=d3d11va",
		"-filter_hw_device", "d3d11va",
		"-f", "lavfi",
		"-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%d:draw_mouse=%d",
			t.Monitor.DXGIIndex, opts.FPS, boolToInt(opts.DrawMouse)),
	}

	// The frames live on the GPU, so they have to be downloaded before any
	// ordinary filter can touch them. `format=bgra` names the layout ddagrab
	// produces; from there ffmpeg converts to whatever the encoder wants.
	filters := []string{"hwdownload", "format=bgra"}
	if t.Kind == KindWindow {
		region, ok := t.windowRegion()
		if !ok {
			return InputSpec{}, errors.New("window is not on a known monitor, so it cannot be cropped out of a duplicated output")
		}
		// A rotated output arrives unrotated, so a rectangle in the desktop's
		// coordinates cannot be mapped onto it with a plain crop.
		if t.Monitor.Rotation != 0 {
			return InputSpec{}, errors.New("cannot crop a window from a rotated monitor with ddagrab")
		}
		x := region.X - t.Monitor.Bounds.X
		y := region.Y - t.Monitor.Bounds.Y
		if region.Width < 2 || region.Height < 2 {
			return InputSpec{}, errors.New("window is almost entirely off screen, so there is nothing to crop")
		}
		filters = append(filters, fmt.Sprintf("crop=%d:%d:%d:%d", region.Width, region.Height, x, y))
		// Window rectangles are arbitrary; H.264's 4:2:0 chroma needs even
		// dimensions, so round down before the encoder sees the frame.
		filters = append(filters, "scale=trunc(iw/2)*2:trunc(ih/2)*2")
	}
	filters = append(filters, rotationFilters(t.Monitor.Rotation)...)
	filters = append(filters, scaleFilters(opts.Scale)...)

	return InputSpec{Args: args, Filters: strings.Join(filters, ","), Grabber: GrabberDDAGrab}, nil
}

// gdigrabSpec builds the GDI input, for a monitor region or a single window.
func (t Target) gdigrabSpec(opts InputOptions) (InputSpec, error) {
	switch t.Kind {
	case KindMonitor:
		b := t.Monitor.Bounds
		if b.Empty() {
			return InputSpec{}, errors.New("monitor has an empty rectangle")
		}
		return InputSpec{
			Args:    gdigrabDesktopArgs(b, opts),
			Filters: strings.Join(scaleFilters(opts.Scale), ","),
			Grabber: GrabberGDigrab,
		}, nil

	case KindWindow:
		// GDI can select a window by title, but that path shares the window's
		// own drawing surface and returns black for anything the GPU
		// composites. The desktop rectangle the window occupies does not have
		// that problem, so a window is captured as a region of the screen.
		// The cost is that a window which is covered by another window shows
		// the covering window instead of its own (invisible) pixels.
		region, ok := t.windowRegion()
		if !ok {
			// Without a monitor snapshot the rectangle is still usable: GDI
			// addresses the virtual desktop by offset and size, and does not
			// need to know which monitor the window is on.
			region = t.Window.Rect
		}
		if region.Empty() {
			return InputSpec{}, errors.New("window has an empty rectangle, so there is nothing to capture")
		}
		if region.Width < 2 || region.Height < 2 {
			return InputSpec{}, errors.New("window is almost entirely off screen, so there is nothing to capture")
		}
		return InputSpec{
			Args:    gdigrabDesktopArgs(region, opts),
			Filters: strings.Join(windowRegionFilters(opts.Scale), ","),
			Grabber: GrabberGDigrab,
		}, nil

	default:
		return InputSpec{}, fmt.Errorf("cannot capture a %s", t.Kind)
	}
}

// gdigrabDesktopArgs opens a rectangle of the composited desktop.
//
// GDI grabs the whole virtual desktop, so the region is selected purely by
// offset and size. No rotation filter is applied: GDI reads the desktop the way
// DWM has already rotated it, which is the opposite of ddagrab.
func gdigrabDesktopArgs(b Rect, opts InputOptions) []string {
	args := []string{
		"-f", "gdigrab",
		"-framerate", strconv.Itoa(opts.FPS),
		"-draw_mouse", strconv.Itoa(boolToInt(opts.DrawMouse)),
		"-offset_x", strconv.Itoa(b.X),
		"-offset_y", strconv.Itoa(b.Y),
		"-video_size", fmt.Sprintf("%dx%d", b.Width, b.Height),
		"-i", "desktop",
	}
	return args
}

// windowRegionFilters rounds a window's arbitrary size down to the even
// dimensions H.264's 4:2:0 chroma requires.
func windowRegionFilters(scale string) []string {
	filters := []string{"scale=trunc(iw/2)*2:trunc(ih/2)*2"}
	return append(filters, scaleFilters(scale)...)
}

// WindowFilters is the filter chain used by the PrintWindow frame source. The
// source already fits inside the configured resolution and produces even
// dimensions, so it must not be scaled up to the display resolution: a 900x700
// window stays 900x700 (rounded down) instead of being blown up to 1080p and
// costing CPU for no extra detail.
func WindowFilters(string) string {
	return "scale=trunc(iw/2)*2:trunc(ih/2)*2"
}

// rotationFilters turns a display rotation into transpose filters.
//
// Desktop Duplication always hands out the display's unrotated surface. The
// portrait monitor used during development is reported by Windows as
// 1080x1920, while ddagrab produces a 1920x1080 image; these filters put it
// back the way the user sees it. 180 needs two transposes because ffmpeg has
// no "rotate half a turn" filter; `hflip,vflip` would work too.
func rotationFilters(rotation int) []string {
	switch rotation {
	case 90:
		return []string{"transpose=1"} // 90 degrees clockwise
	case 180:
		return []string{"transpose=1", "transpose=1"}
	case 270:
		return []string{"transpose=2"} // 90 degrees counter-clockwise
	default:
		return nil
	}
}

// scaleFilters resizes the frames when the settings ask for it.
//
// An explicit scale always ends with a rounding step because H.264 with
// 4:2:0 chroma needs even width and height, and users type odd numbers.
func scaleFilters(scale string) []string {
	scale = strings.TrimSpace(scale)
	if scale == "" {
		return nil
	}
	return []string{"scale=" + scale, "scale=trunc(iw/2)*2:trunc(ih/2)*2"}
}

// escapeFFmpegValue escapes the characters ffmpeg treats as syntax inside an
// option value, so a window title like "C:\tmp: notes" survives intact.
func escapeFFmpegValue(v string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`,`, `\,`,
		`'`, `\'`,
		`%`, `\%`,
	)
	return replacer.Replace(v)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

//go:build windows

package capture

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file holds the raw Win32 bindings this package needs.
//
// Why hand written bindings instead of a dependency? golang.org/x/sys/windows
// covers the common calls (EnumWindows, DwmGetWindowAttribute, ...) but not the
// display configuration API, and that API is the only correct way to learn
// which DXGI output number ffmpeg's ddagrab filter should be pointed at. Pulling
// in a third party Win32 wrapper for two functions would be a worse trade.
//
// Calling convention: proc.Call returns the raw result plus the last error,
// and never panics, so every wrapper here turns a failure into a plain error.
// syscall.SyscallN does not "throw"; a crash would mean a bug in the struct
// layout below, which is why the structs carry their expected sizes and there
// is a test that checks them.

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procGetWindowRect       = user32.NewProc("GetWindowRect")
	procGetClientRect       = user32.NewProc("GetClientRect")
	procClientToScreen      = user32.NewProc("ClientToScreen")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetClassNameW       = user32.NewProc("GetClassNameW")
	procIsIconic            = user32.NewProc("IsIconic")
	procGetWindowLongW      = user32.NewProc("GetWindowLongW")

	procQueryDisplayConfig          = user32.NewProc("QueryDisplayConfig")
	procGetDisplayConfigBufferSizes = user32.NewProc("GetDisplayConfigBufferSizes")

	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
)

// windowLongExStyle is GWL_EXSTYLE. The constant is -20 in the Windows headers,
// written here as an unsigned value because that is what the callback wants.
const windowLongExStyle = ^uintptr(19)

// wsExToolWindow marks a window that is not meant to appear in the taskbar:
// invisible helpers, IME composition windows, drop shadow hosts. They are
// excluded from the window list so the picker stays readable.
const wsExToolWindow = 0x00000080

// dwmwaExtendedFrameBounds is the DWM attribute that gives a window's visible
// frame. GetWindowRect lies about maximised windows by about seven pixels on
// each side, which is enough to make a maximised window look fullscreen.
const dwmwaExtendedFrameBounds = 9

// monitorInfoEx is MONITORINFOEXW.
type monitorInfoEx struct {
	CbSize    uint32
	RcMonitor windows.Rect
	RcWork    windows.Rect
	DwFlags   uint32
	SzDevice  [32]uint16
}

const monitorInfoPrimary = 0x00000001

// pointL is POINTL, used for desktop coordinates in the display config API.
type pointL struct {
	X int32
	Y int32
}

// The display configuration structures below mirror the Windows SDK exactly.
// The comments give each one's byte size; displayConfigStructSizes in
// win_display_test.go asserts them, because a struct whose padding is wrong
// makes QueryDisplayConfig scribble over memory instead of failing cleanly.

// luID is LUID (8 bytes).
type luID struct {
	LowPart  uint32
	HighPart int32
}

// displayConfigPathSourceInfo is DISPLAYCONFIG_PATH_SOURCE_INFO (20 bytes).
type displayConfigPathSourceInfo struct {
	AdapterID   luID
	ID          uint32
	ModeInfoIdx uint32
	StatusFlags uint32
}

// displayConfigRational is DISPLAYCONFIG_RATIONAL (8 bytes).
type displayConfigRational struct {
	Numerator   uint32
	Denominator uint32
}

// displayConfigPathTargetInfo is DISPLAYCONFIG_PATH_TARGET_INFO (48 bytes).
type displayConfigPathTargetInfo struct {
	AdapterID        luID
	ID               uint32
	ModeInfoIdx      uint32
	OutputTechnology uint32
	Rotation         uint32
	Scaling          uint32
	RefreshRate      displayConfigRational
	ScanLineOrdering uint32
	TargetAvailable  int32
	StatusFlags      uint32
}

// displayConfigPathInfo is DISPLAYCONFIG_PATH_INFO (72 bytes).
type displayConfigPathInfo struct {
	SourceInfo displayConfigPathSourceInfo
	TargetInfo displayConfigPathTargetInfo
	Flags      uint32
}

// displayConfig2DRegion is DISPLAYCONFIG_2DREGION (8 bytes).
type displayConfig2DRegion struct {
	Cx uint32
	Cy uint32
}

// displayConfigVideoSignalInfo is DISPLAYCONFIG_VIDEO_SIGNAL_INFO (48 bytes).
type displayConfigVideoSignalInfo struct {
	PixelRate        uint64
	HSyncFreq        displayConfigRational
	VSyncFreq        displayConfigRational
	ActiveSize       displayConfig2DRegion
	TotalSize        displayConfig2DRegion
	VideoStandard    uint32
	ScanLineOrdering uint32
}

// displayConfigTargetMode is DISPLAYCONFIG_TARGET_MODE (48 bytes).
type displayConfigTargetMode struct {
	VideoSignalInfo displayConfigVideoSignalInfo
}

// displayConfigSourceMode is DISPLAYCONFIG_SOURCE_MODE (20 bytes).
type displayConfigSourceMode struct {
	Width       uint32
	Height      uint32
	PixelFormat uint32
	Position    pointL
}

// displayConfigDesktopImageInfo is DISPLAYCONFIG_DESKTOP_IMAGE_INFO (40 bytes).
type displayConfigDesktopImageInfo struct {
	PathSourceSize     displayConfig2DRegion
	DesktopImageClip   windows.Rect
	DesktopImageRegion windows.Rect
}

// displayConfigModeInfo is DISPLAYCONFIG_MODE_INFO (64 bytes).
//
// The SDK declares the last member as an unnamed union of the three mode types.
// Go has no unions, so the struct carries the largest member and the accessors
// below reinterpret its first bytes, which is exactly what the C union does.
type displayConfigModeInfo struct {
	InfoType   uint32
	ID         uint32
	AdapterID  luID
	TargetMode displayConfigTargetMode
}

// sourceMode reinterprets the union as a source mode. Only valid when InfoType
// is displayConfigModeInfoTypeSource.
func (m *displayConfigModeInfo) sourceMode() *displayConfigSourceMode {
	return (*displayConfigSourceMode)(unsafe.Pointer(&m.TargetMode))
}

// DISPLAYCONFIG_MODE_INFO_TYPE values.
const (
	displayConfigModeInfoTypeSource uint32 = 1
	displayConfigModeInfoTypeTarget uint32 = 2
)

// DISPLAYCONFIG_ROTATION values.
const (
	displayConfigRotationIdentity uint32 = 1
	displayConfigRotation90       uint32 = 2
	displayConfigRotation180      uint32 = 3
	displayConfigRotation270      uint32 = 4
)

// qdcOnlyActivePaths asks for the display paths that are actually in use.
const qdcOnlyActivePaths uint32 = 0x00000002

// displayPath is the part of a display path this package cares about.
type displayPath struct {
	// AdapterID and TargetID identify the DXGI output. TargetID is a
	// driver-assigned identifier, which is emphatically not the same thing as
	// the zero-based output index the DXGI runtime hands out.
	AdapterID luID
	TargetID  uint32
	// Desktop is the source rectangle in virtual desktop coordinates, which is
	// the same rectangle GetMonitorInfo reports.
	Desktop Rect
	// ActiveSize is the resolution on the wire, before rotation.
	ActiveSize Rect
	// Rotation in degrees: 0, 90, 180 or 270.
	Rotation int
	// OutputTechnology is DISPLAYCONFIG_VIDEO_OUTPUT_TECHNOLOGY: internal,
	// HDMI, DisplayPort, virtual and so on.
	OutputTechnology uint32
}

// luidValue packs an adapter LUID into a single comparable number.
func luidValue(l luID) uint64 {
	return uint64(uint32(l.HighPart))<<32 | uint64(l.LowPart)
}

// DisplayPaths returns the Windows display configuration paths.
//
// It exists for diagnostics. These paths are the only place where adapter
// identity, output rotation and the desktop rectangle are visible together, so
// when a capture lands on the wrong screen this is the data that explains why.
func DisplayPaths() ([]DisplayPath, error) {
	paths, err := queryDisplayPaths()
	if err != nil {
		return nil, err
	}

	out := make([]DisplayPath, 0, len(paths))
	for _, p := range paths {
		out = append(out, DisplayPath{
			AdapterLUID:      luidValue(p.AdapterID),
			TargetID:         p.TargetID,
			Desktop:          p.Desktop,
			ActiveSize:       p.ActiveSize,
			Rotation:         p.Rotation,
			OutputTechnology: p.OutputTechnology,
		})
	}
	return out, nil
}

// queryDisplayPaths returns the active display paths.
//
// The sizes come from GetDisplayConfigBufferSizes rather than from a
// QueryDisplayConfig call with null arrays. That trick works for the
// EnumDisplayMonitors family but not here: this API rejects a zero sized buffer
// with ERROR_INVALID_PARAMETER, which reads as "you called it wrong" when the
// real answer is "ask for the sizes first".
func queryDisplayPaths() ([]displayPath, error) {
	var numPaths, numModes uint32

	if r, _, _ := procGetDisplayConfigBufferSizes.Call(
		uintptr(qdcOnlyActivePaths),
		uintptr(unsafe.Pointer(&numPaths)),
		uintptr(unsafe.Pointer(&numModes)),
	); r != 0 {
		return nil, errorCode(r, "GetDisplayConfigBufferSizes")
	}
	if numPaths == 0 {
		return nil, nil
	}

	paths := make([]displayConfigPathInfo, numPaths)
	modes := make([]displayConfigModeInfo, numModes)

	r, _, _ := procQueryDisplayConfig.Call(
		uintptr(qdcOnlyActivePaths),
		uintptr(unsafe.Pointer(&numPaths)),
		uintptr(unsafe.Pointer(&paths[0])),
		uintptr(unsafe.Pointer(&numModes)),
		uintptr(unsafe.Pointer(&modes[0])),
		uintptr(unsafe.Pointer(nil)),
	)
	if r != 0 {
		return nil, errorCode(r, "QueryDisplayConfig")
	}

	out := make([]displayPath, 0, len(paths))
	for i := range paths {
		p := &paths[i]

		// A path whose target is not available is a display that is
		// configured but not currently attached.
		if p.TargetInfo.TargetAvailable == 0 {
			continue
		}

		dp := displayPath{
			AdapterID:        p.TargetInfo.AdapterID,
			TargetID:         p.TargetInfo.ID,
			Rotation:         rotationDegrees(p.TargetInfo.Rotation),
			OutputTechnology: p.TargetInfo.OutputTechnology,
		}

		if idx := int(p.SourceInfo.ModeInfoIdx); idx >= 0 && idx < len(modes) && modes[idx].InfoType == displayConfigModeInfoTypeSource {
			sm := modes[idx].sourceMode()
			dp.Desktop = Rect{
				X:      int(sm.Position.X),
				Y:      int(sm.Position.Y),
				Width:  int(sm.Width),
				Height: int(sm.Height),
			}
		}
		if idx := int(p.TargetInfo.ModeInfoIdx); idx >= 0 && idx < len(modes) && modes[idx].InfoType == displayConfigModeInfoTypeTarget {
			active := modes[idx].TargetMode.VideoSignalInfo.ActiveSize
			dp.ActiveSize = Rect{Width: int(active.Cx), Height: int(active.Cy)}
		}

		out = append(out, dp)
	}
	return out, nil
}

// rotationDegrees converts DISPLAYCONFIG_ROTATION to degrees.
func rotationDegrees(rotation uint32) int {
	switch rotation {
	case displayConfigRotation90:
		return 90
	case displayConfigRotation180:
		return 180
	case displayConfigRotation270:
		return 270
	default:
		return 0
	}
}

// errorCode turns a Win32 return value into an error with a readable message.
func errorCode(code uintptr, what string) error {
	errno := windows.Errno(code)
	if errno == 0 {
		return nil
	}
	return &win32Error{what: what, err: errno}
}

type win32Error struct {
	what string
	err  error
}

func (e *win32Error) Error() string {
	return e.what + ": " + e.err.Error()
}

func (e *win32Error) Unwrap() error { return e.err }

var dpiOnce sync.Once

// EnableDPIAwareness makes this process report real physical pixels.
//
// Without it Windows lies to GetWindowRect and friends, scaling the numbers by
// the display's DPI setting, and a window on a 150% display would look like it
// covers only two thirds of its monitor. It must be called before the process
// creates its first window, so main calls it before anything else, and the
// sync.Once makes the later call from the resolver a cheap no-op.
//
// Per monitor v2 is the good mode; the older calls are fallbacks for Windows
// versions that predate it.
func EnableDPIAwareness() {
	dpiOnce.Do(func() {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 is the pseudo handle -4.
		if r, _, _ := procSetProcessDpiAwarenessContext.Call(^uintptr(3)); r != 0 {
			return
		}
		if r, _, _ := procSetProcessDPIAware.Call(); r != 0 {
			return
		}
		// Failing both is not fatal: capture still works, coordinates are just
		// scaled. There is nobody to report to at this point in startup, so
		// the resolver logs the DPI it ends up seeing instead.
	})
}

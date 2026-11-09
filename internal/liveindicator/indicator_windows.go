//go:build windows

package liveindicator

import (
	"log/slog"
	"math"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Why a hand written layered window instead of a Wails/Svelte overlay?
//
// The indicator has to exist while the settings UI is closed, must never take
// focus, and must stay above ordinary windows. A top-level WS_EX_LAYERED popup
// owned by the tray process is the smallest thing that does all three: no
// webview, no extra process, and only a few hundred bytes of bitmap.

const (
	liveIndicatorClass = "StreamyLiveIndicator"

	wsPopup = 0x80000000

	wsExTopmost     = 0x00000008
	wsExTransparent = 0x00000020
	wsExToolWindow  = 0x00000080
	wsExLayered     = 0x00080000
	wsExNoActivate  = 0x08000000

	swHide = 0

	// HWND_TOPMOST is (HWND)-1.
	hwndTopmost = ^uintptr(0)

	swpNoSize        = 0x0001
	swpNoMove        = 0x0002
	swpNoActivate    = 0x0010
	swpShowWindow    = 0x0040
	swpNoOwnerZOrder = 0x0200

	ulwAlpha   = 0x00000002
	acSrcOver  = 0x00
	acSrcAlpha = 0x01

	wmDestroy   = 0x0002
	wmClose     = 0x0010
	wmTimer     = 0x0113
	wmNCHitTest = 0x0084
	wmApp       = 0x8000
	wmRefresh   = wmApp + 1

	// HTTRANSPARENT is (LRESULT)-1.
	htTransparent = ^uintptr(0)

	timerBlink      = 1
	blinkIntervalMS = 600

	dotSize96   = 16
	dotMargin96 = 24

	// WDA_EXCLUDEFROMCAPTURE keeps the cue out of screen recordings while
	// leaving it visible to the streamer. Supported since Windows 10 2004.
	wdaExcludeFromCapture = 0x11

	spiGetWorkArea = 0x0030

	// Text rendering constants
	transparent = 1
	ansiCharset = 0
	outDefaultPrecis = 0
	clipDefaultPrecis = 0
	defaultQuality = 0
	defaultPitch  = 0
	ffDontCare    = 0
	fwBold        = 700
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procSetTimer                 = user32.NewProc("SetTimer")
	procKillTimer                = user32.NewProc("KillTimer")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procSetWindowDisplayAffinity = user32.NewProc("SetWindowDisplayAffinity")
	procGetDpiForWindow          = user32.NewProc("GetDpiForWindow")
	procGetDC                    = user32.NewProc("GetDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")
	procSystemParametersInfoW    = user32.NewProc("SystemParametersInfoW")
	procUpdateLayeredWindow      = user32.NewProc("UpdateLayeredWindow")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procCreateFontIndirectW  = gdi32.NewProc("CreateFontIndirectW")
	procSetBkMode            = gdi32.NewProc("SetBkMode")
	procSetTextColor         = gdi32.NewProc("SetTextColor")
	procTextOutW             = gdi32.NewProc("TextOutW")
	procGetTextExtentPoint32W = gdi32.NewProc("GetTextExtentPoint32W")
)

// wndProcCallback must be created once: every RegisterClassEx call has to use
// the same callback address.
var wndProcCallback = windows.NewCallback(wndProc)

// windowsInUse maps a window handle to its Indicator. sync.Map is fine here:
// there is normally exactly one entry, written once on the window thread.
var windowsInUse sync.Map

type Indicator struct {
	logger *slog.Logger

	// mu guards the state read from the window thread and written by callers.
	mu           sync.Mutex
	started      bool
	closed       bool
	hwnd         uintptr
	position     string
	visible      bool
	blinkOn      bool
	viewerCount  int
	showViewerCount bool

	// These handles belong to the window thread and are only touched there.
	memDC       uintptr
	bitmap      uintptr
	bits        unsafe.Pointer
	bitmapSize  int
	bitmapWidth int
	font        windows.Handle

	done      chan struct{}
	closeOnce sync.Once
}

// New creates an indicator. No window or thread is created until Show is
// called, so an idle Streamy does not pay for the overlay.
func New(logger *slog.Logger) Controller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Indicator{
		logger:           logger,
		position:         TopRight,
		done:             make(chan struct{}),
		showViewerCount:  true, // default to on, can be overridden by config
	}
}

func (i *Indicator) Show(position string) {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return
	}
	i.position = normalizePosition(position)
	i.visible = true
	start := !i.started
	i.started = true
	hwnd := i.hwnd
	i.mu.Unlock()

	if start {
		// The new thread reads position/visible after the window exists.
		go i.run()
		return
	}
	if hwnd != 0 && !post(hwnd, wmRefresh) {
		i.logger.Warn("could not update the live indicator")
	}
}

func (i *Indicator) Hide() {
	i.mu.Lock()
	i.visible = false
	hwnd := i.hwnd
	i.mu.Unlock()
	if hwnd != 0 {
		post(hwnd, wmRefresh)
	}
}

func (i *Indicator) SetViewerCount(count int) {
	i.mu.Lock()
	if !i.showViewerCount {
		count = 0
	}
	if i.viewerCount == count {
		i.mu.Unlock()
		return
	}
	i.viewerCount = count
	hwnd := i.hwnd
	started := i.started
	i.mu.Unlock()

	if started && hwnd != 0 {
		post(hwnd, wmRefresh)
	}
}

func (i *Indicator) SetShowViewerCount(enabled bool) {
	i.mu.Lock()
	if i.showViewerCount == enabled {
		i.mu.Unlock()
		return
	}
	i.showViewerCount = enabled
	hwnd := i.hwnd
	started := i.started
	i.mu.Unlock()

	if started && hwnd != 0 {
		post(hwnd, wmRefresh)
	}
}

func (i *Indicator) Close() {
	i.closeOnce.Do(func() {
		i.mu.Lock()
		i.closed = true
		i.visible = false
		started := i.started
		hwnd := i.hwnd
		i.mu.Unlock()

		if !started {
			return
		}
		if hwnd != 0 {
			_ = post(hwnd, wmClose)
		}
		select {
		case <-i.done:
		case <-time.After(2 * time.Second):
			i.logger.Warn("live indicator did not close in time")
		}
	})
}

// run owns a dedicated OS thread: the window and its message queue belong to
// the thread that created them. LockOSThread keeps the Go scheduler from
// moving the loop to another thread.
func (i *Indicator) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(i.done)

	i.mu.Lock()
	closed := i.closed
	i.mu.Unlock()
	if closed {
		return
	}

	instance, _, _ := procGetModuleHandleW.Call(0)
	className, err := windows.UTF16PtrFromString(liveIndicatorClass)
	if err != nil {
		i.logger.Warn("could not prepare the live indicator window class", "error", err)
		return
	}

	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   wndProcCallback,
		Instance:  windows.Handle(instance),
		ClassName: className,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		// 1410 is ERROR_CLASS_ALREADY_EXISTS. A previous indicator in the
		// same process is the only way this can happen, and the class is
		// still usable, so it is not fatal.
		if errno, ok := callErr.(syscall.Errno); !ok || errno != 1410 {
			i.logger.Warn("could not register the live indicator window", "error", callErr)
			return
		}
	}

	exStyle := uintptr(wsExTopmost | wsExTransparent | wsExToolWindow | wsExLayered | wsExNoActivate)
	hwnd, _, callErr := procCreateWindowExW.Call(
		exStyle,
		uintptr(unsafe.Pointer(className)),
		0, // no window title
		wsPopup,
		0, 0, 0, 0,
		0, 0,
		instance,
		0,
	)
	if hwnd == 0 {
		i.logger.Warn("could not create the live indicator window", "error", callErr)
		return
	}
	handle := windows.Handle(hwnd)
	windowsInUse.Store(handle, i)
	defer windowsInUse.Delete(handle)

	// Keep the cue out of screen recordings while leaving it visible on the
	// desktop. This needs the window to exist; failure is non-fatal on older
	// Windows builds, where the dot simply appears in the stream too.
	if ok, _, _ := procSetWindowDisplayAffinity.Call(hwnd, wdaExcludeFromCapture); ok == 0 {
		i.logger.Debug("live indicator cannot be excluded from capture on this Windows version")
	}

	// Create the font for viewer count text.
	i.createFont(hwnd)

	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		procDestroyWindow.Call(hwnd)
		return
	}
	i.hwnd = hwnd
	visible := i.visible
	i.mu.Unlock()

	if visible {
		i.refresh(hwnd)
	}

	var msg msgT
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	i.mu.Lock()
	i.hwnd = 0
	i.mu.Unlock()
	i.releaseBitmap()
}

// createFont creates a font for rendering the viewer count.
func (i *Indicator) createFont(hwnd uintptr) {
	dpi := 96.0
	if dpiValue, _, _ := procGetDpiForWindow.Call(hwnd); dpiValue > 0 {
		dpi = float64(dpiValue)
	}
	// Font height in logical units (negative for character height).
	fontHeight := int32(-math.Round(14 * dpi / 96))
	if fontHeight > -8 {
		fontHeight = -8
	}

	lf := loGFont{
		Height:         fontHeight,
		Width:          0,
		Escapement:     0,
		Orientation:    0,
		Weight:         fwBold,
		Italic:         0,
		Underline:      0,
		StrikeOut:      0,
		CharSet:        ansiCharset,
		OutPrecision:   outDefaultPrecis,
		ClipPrecision:  clipDefaultPrecis,
		Quality:        defaultQuality,
		PitchAndFamily: byte(defaultPitch | ffDontCare),
	}
	copy(lf.FaceName[:], utf16FromString("Segoe UI"))

	font, _, _ := procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&lf)))
	i.font = windows.Handle(font)
}

// refresh applies the latest position/visibility, then starts or stops the
// blink timer. It always runs on the window thread.
func (i *Indicator) refresh(hwnd uintptr) {
	i.mu.Lock()
	visible := i.visible && !i.closed
	position := i.position
	viewerCount := i.viewerCount
	showCount := i.showViewerCount
	i.mu.Unlock()

	if !visible {
		procKillTimer.Call(hwnd, timerBlink)
		procShowWindow.Call(hwnd, swHide)
		return
	}

	size, margin := metrics(hwnd)
	// Calculate total width: dot + gap + text (if any)
	totalWidth := size
	if showCount && viewerCount > 0 {
		text := strconv.Itoa(viewerCount)
		textWidth := i.measureText(text)
		totalWidth = size + 6 + textWidth // 6px gap between dot and text
	}
	i.ensureBitmap(size, totalWidth)
	x, y := positionFor(workArea(), totalWidth, margin, position)

	procSetWindowPos.Call(
		hwnd, hwndTopmost,
		uintptr(int32(x)), uintptr(int32(y)),
		uintptr(int32(totalWidth)), uintptr(int32(size)),
		swpNoActivate|swpShowWindow|swpNoOwnerZOrder,
	)
	i.mu.Lock()
	i.blinkOn = true
	i.mu.Unlock()
	i.paint(hwnd, x, y, totalWidth, 255)
	procSetTimer.Call(hwnd, timerBlink, blinkIntervalMS, 0)
}

// onBlink toggles the dot between opaque and transparent. Re-asserting
// HWND_TOPMOST here is deliberate: another always-on-top app can otherwise end
// up above the cue after a while.
func (i *Indicator) onBlink(hwnd uintptr) {
	i.mu.Lock()
	on := !i.blinkOn
	i.blinkOn = on
	visible := i.visible && !i.closed
	position := i.position
	viewerCount := i.viewerCount
	showCount := i.showViewerCount
	i.mu.Unlock()
	if !visible {
		return
	}

	size, margin := metrics(hwnd)
	totalWidth := size
	if showCount && viewerCount > 0 {
		text := strconv.Itoa(viewerCount)
		textWidth := i.measureText(text)
		totalWidth = size + 6 + textWidth
	}
	i.ensureBitmap(size, totalWidth)
	x, y := positionFor(workArea(), totalWidth, margin, position)
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	if on {
		i.paint(hwnd, x, y, totalWidth, 255)
	} else {
		i.paint(hwnd, x, y, totalWidth, 0)
	}
}

func (i *Indicator) paint(hwnd uintptr, x, y, width int, alpha byte) {
	if i.memDC == 0 || i.bitmap == 0 {
		return
	}
	dst := pointT{X: int32(x), Y: int32(y)}
	dimensions := sizeT{Cx: int32(width), Cy: int32(i.bitmapSize)}
	src := pointT{}
	blend := blendFunction{
		BlendOp:             acSrcOver,
		SourceConstantAlpha: alpha,
		AlphaFormat:         acSrcAlpha,
	}
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return
	}
	defer procReleaseDC.Call(0, screenDC)

	procUpdateLayeredWindow.Call(
		hwnd, screenDC,
		uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&dimensions)),
		i.memDC, uintptr(unsafe.Pointer(&src)),
		0, uintptr(unsafe.Pointer(&blend)), ulwAlpha,
	)
}

// ensureBitmap creates the 32-bit premultiplied BGRA bitmap the first time it
// is needed, and again only when the DPI (and therefore the size) changes.
// width is the total width including the dot and optional viewer count text.
func (i *Indicator) ensureBitmap(dotSize, width int) {
	if i.bitmap != 0 && i.bitmapSize == dotSize && i.bitmapWidth == width {
		return
	}
	i.releaseBitmap()

	memDC, _, _ := procCreateCompatibleDC.Call(0)
	if memDC == 0 {
		i.logger.Warn("could not create a DC for the live indicator")
		return
	}
	info := bitmapInfo{
		Header: bitmapInfoHeader{
			Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			Width:       int32(width),
			Height:      int32(dotSize),
			Planes:      1,
			BitCount:    32,
			Compression: 0, // BI_RGB
		},
	}
	var bits unsafe.Pointer
	bitmap, _, callErr := procCreateDIBSection.Call(
		memDC, uintptr(unsafe.Pointer(&info)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0,
	)
	if bitmap == 0 || bits == nil {
		procDeleteDC.Call(memDC)
		i.logger.Warn("could not create the live indicator bitmap", "error", callErr)
		return
	}
	procSelectObject.Call(memDC, bitmap)

	i.memDC = memDC
	i.bitmap = bitmap
	i.bits = bits
	i.bitmapSize = dotSize
	i.bitmapWidth = width

	// Draw the dot and text into the bitmap
	i.drawBitmap(dotSize, width)
}

func (i *Indicator) drawBitmap(dotSize, width int) {
	if i.bits == nil {
		return
	}

	// Clear the bitmap first
	pixels := make([]byte, width*dotSize*4)
	copy(unsafe.Slice((*byte)(i.bits), len(pixels)), pixels)

	// Draw the dot at the left
	dotPixels := dotPixels(dotSize)
	for y := 0; y < dotSize; y++ {
		srcOffset := y * dotSize * 4
		dstOffset := y * width * 4
		copy(unsafe.Slice((*byte)(i.bits), len(pixels))[dstOffset:dstOffset+dotSize*4], dotPixels[srcOffset:srcOffset+dotSize*4])
	}

	// Draw the viewer count text if needed
	i.mu.Lock()
	viewerCount := i.viewerCount
	showCount := i.showViewerCount
	font := i.font
	i.mu.Unlock()

	if showCount && viewerCount > 0 && font != 0 {
		text := strconv.Itoa(viewerCount)
		i.drawText(text, dotSize, width)
	}
}

func (i *Indicator) drawText(text string, dotSize, width int) {
	if i.memDC == 0 || i.font == 0 {
		return
	}

	// Select the font
	oldFont, _, _ := procSelectObject.Call(i.memDC, uintptr(i.font))
	if oldFont == 0 {
		return
	}
	defer procSelectObject.Call(i.memDC, oldFont)

	// Set transparent background and white text color
	procSetBkMode.Call(i.memDC, transparent)
	procSetTextColor.Call(i.memDC, 0xFFFFFF) // White text (BGR: 0xFFFFFF)

	// Convert text to UTF-16
	text16, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return
	}

	// Calculate text position (vertically centered, after the dot + gap)
	textX := dotSize + 6 // 6px gap after the dot
	textY := (dotSize - i.getFontHeight()) / 2
	if textY < 0 {
		textY = 0
	}

	procTextOutW.Call(i.memDC, uintptr(textX), uintptr(textY), uintptr(unsafe.Pointer(text16)), uintptr(len(text)))
}

func (i *Indicator) getFontHeight() int {
	if i.memDC == 0 || i.font == 0 {
		return 0
	}
	oldFont, _, _ := procSelectObject.Call(i.memDC, uintptr(i.font))
	if oldFont == 0 {
		return 0
	}
	defer procSelectObject.Call(i.memDC, oldFont)

	var size sizeT
	text := "0"
	text16, _ := windows.UTF16PtrFromString(text)
	procGetTextExtentPoint32W.Call(i.memDC, uintptr(unsafe.Pointer(text16)), uintptr(len(text)), uintptr(unsafe.Pointer(&size)))
	return int(size.Cy)
}

func (i *Indicator) measureText(text string) int {
	if i.memDC == 0 || i.font == 0 {
		return 0
	}
	oldFont, _, _ := procSelectObject.Call(i.memDC, uintptr(i.font))
	if oldFont == 0 {
		return 0
	}
	defer procSelectObject.Call(i.memDC, oldFont)

	var size sizeT
	text16, _ := windows.UTF16PtrFromString(text)
	procGetTextExtentPoint32W.Call(i.memDC, uintptr(unsafe.Pointer(text16)), uintptr(len(text)), uintptr(unsafe.Pointer(&size)))
	return int(size.Cx)
}

func (i *Indicator) releaseBitmap() {
	// The bitmap is still selected into memDC. Delete the DC first: a selected
	// bitmap cannot be deleted, and DeleteDC does not own it.
	if i.memDC != 0 {
		procDeleteDC.Call(i.memDC)
		i.memDC = 0
	}
	if i.bitmap != 0 {
		procDeleteObject.Call(i.bitmap)
		i.bitmap = 0
	}
	if i.font != 0 {
		procDeleteObject.Call(uintptr(i.font))
		i.font = 0
	}
	i.bits = nil
	i.bitmapSize = 0
	i.bitmapWidth = 0
}

func (i *Indicator) handleMessage(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmRefresh:
		i.refresh(hwnd)
		return 0
	case wmTimer:
		if wparam == timerBlink {
			i.onBlink(hwnd)
		}
		return 0
	case wmNCHitTest:
		// Belt and braces with WS_EX_TRANSPARENT: clicks belong to whatever is
		// underneath the dot.
		return htTransparent
	case wmClose:
		procKillTimer.Call(hwnd, timerBlink)
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return ret
}

// wndProc is the single native callback shared by every indicator window. The
// owning *Indicator is looked up in windowsInUse: a Go pointer cannot be passed
// through a pointer sized window word safely, and the registry is also easier
// to reason about than GWLP_USERDATA.
func wndProc(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) uintptr {
	value, ok := windowsInUse.Load(hwnd)
	if !ok {
		ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
		return ret
	}
	return value.(*Indicator).handleMessage(uintptr(hwnd), msg, wparam, lparam)
}

// metrics scales the dot with the monitor DPI. A 16 pixel dot is comfortable
// at 100%; the same perceived size at 200% is 32 pixels.
func metrics(hwnd uintptr) (size, margin int) {
	dpi := 96.0
	if dpiValue, _, _ := procGetDpiForWindow.Call(hwnd); dpiValue > 0 {
		dpi = float64(dpiValue)
	}
	size = int(math.Round(dotSize96 * dpi / 96))
	margin = int(math.Round(dotMargin96 * dpi / 96))
	if size < 8 {
		size = 8
	}
	if margin < 4 {
		margin = 4
	}
	return size, margin
}

// workArea is the primary monitor's desktop minus the taskbar, so a bottom
// corner never hides behind it.
func workArea() windows.Rect {
	var rect windows.Rect
	if ok, _, _ := procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&rect)), 0); ok != 0 &&
		rect.Right > rect.Left && rect.Bottom > rect.Top {
		return rect
	}
	return windows.Rect{Right: 1920, Bottom: 1080}
}

func positionFor(area windows.Rect, size, margin int, position string) (int, int) {
	left := int(area.Left) + margin
	right := int(area.Right) - size - margin
	top := int(area.Top) + margin
	bottom := int(area.Bottom) - size - margin

	switch normalizePosition(position) {
	case TopLeft:
		return left, top
	case BottomLeft:
		return left, bottom
	case BottomRight:
		return right, bottom
	default:
		return right, top
	}
}

func post(hwnd uintptr, msg uintptr) bool {
	ok, _, _ := procPostMessageW.Call(hwnd, msg, 0, 0)
	return ok != 0
}

// dotPixels renders the cue as premultiplied BGRA pixels. The circle is
// supersampled 4x4 per pixel, which is cheap at this size and gives clean edges
// on any background.
//
// The DIB is bottom-up, but the dot is vertically symmetric, so the row order
// does not matter.
func dotPixels(size int) []byte {
	pixels := make([]byte, size*size*4)
	if size <= 0 {
		return pixels
	}

	center := float64(size) / 2
	radius := float64(size) / 2
	dotRadius := radius * 0.72
	const ringInset = 1.0 // leave a transparent pixel around the ring

	// A bright red disc over a dark translucent ring reads well on light and
	// dark desktops alike.
	var redR, redG, redB = 255.0, 59.0, 48.0

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var dotCoverage, ringCoverage float64
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					px := float64(x) + (float64(sx)+0.5)/4
					py := float64(y) + (float64(sy)+0.5)/4
					distance := math.Hypot(px-center, py-center)
					if distance <= dotRadius {
						dotCoverage++
					}
					if distance <= radius-ringInset {
						ringCoverage++
					}
				}
			}
			dotCoverage /= 16
			ringCoverage /= 16

			ringAlpha := ringCoverage * 0.55
			outAlpha := dotCoverage + ringAlpha*(1-dotCoverage)
			index := (y*size + x) * 4
			if outAlpha <= 0 {
				continue
			}
			pixels[index+0] = clampByte(redB * dotCoverage) // blue
			pixels[index+1] = clampByte(redG * dotCoverage) // green
			pixels[index+2] = clampByte(redR * dotCoverage) // red
			pixels[index+3] = clampByte(outAlpha * 255)     // alpha
		}
	}
	return pixels
}

func clampByte(value float64) byte {
	if value <= 0 {
		return 0
	}
	if value >= 255 {
		return 255
	}
	return byte(math.Round(value))
}

// --- raw Win32 structures -------------------------------------------------

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msgT struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type pointT struct {
	X, Y int32
}

type sizeT struct {
	Cx, Cy int32
}

type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type rgbQuad struct {
	Blue, Green, Red, Reserved byte
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]rgbQuad
}

// loGFont mirrors the LOGFONTW structure for CreateFontIndirectW.
type loGFont struct {
	Height         int32
	Width          int32
	Escapement     int32
	Orientation    int32
	Weight         int32
	Italic         byte
	Underline      byte
	StrikeOut      byte
	CharSet        byte
	OutPrecision   byte
	ClipPrecision  byte
	Quality        byte
	PitchAndFamily byte
	FaceName       [32]uint16
}

// utf16FromString converts a Go string to a null-terminated UTF-16 slice for
// populating the LOGFONTW FaceName field.
func utf16FromString(s string) []uint16 {
	var result []uint16
	for _, r := range s {
		if r <= 0xFFFF {
			result = append(result, uint16(r))
		} else {
			r -= 0x10000
			result = append(result, uint16(0xD800|(r>>10)))
			result = append(result, uint16(0xDC00|(r&0x3FF)))
		}
	}
	return result
}

//go:build windows

package capture

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// WindowCaptureSupported reports whether this build can send a window's own
// pixels instead of a crop of the composited desktop.
func WindowCaptureSupported() bool { return true }

var (
	procCreateDIBSection = gdi32.NewProc("CreateDIBSection")
	procGdiFlush         = gdi32.NewProc("GdiFlush")
	procGetDC            = user32.NewProc("GetDC")
	procReleaseDC        = user32.NewProc("ReleaseDC")
	procIsWindow         = user32.NewProc("IsWindow")
	procGetWindowDC      = user32.NewProc("GetWindowDC")
	procGetCursorInfo    = user32.NewProc("GetCursorInfo")
	procDrawIconEx       = user32.NewProc("DrawIconEx")
)

const (
	cursorShowing = 0x00000001
	diNormal      = 0x0003
)

// cursorInfo is CURSORINFO. The size field must be filled in before the call.
type cursorInfo struct {
	CbSize      uint32
	Flags       uint32
	HCursor     uintptr
	PtScreenPos cursorPoint
}

type cursorPoint struct {
	X int32
	Y int32
}

type streamBitmapInfo struct {
	Header streamBitmapInfoHeader
	Colors [1]uint32
}

type streamBitmapInfoHeader struct {
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

// WindowFrameSource captures one HWND with PrintWindow and serves the raw BGRA
// frames on a loopback TCP socket. ffmpeg opens that socket as a rawvideo
// input, so the encoder never sees the desktop underneath the window.
//
// A window can be dragged or resized while streaming. The source keeps the
// size it started with; changing size would require restarting ffmpeg (the
// picker already offers a switch, and a restarted capture would interrupt the
// stream). The window's own content is still rendered, scaled into the original
// frame.
type WindowFrameSource struct {
	hwnd uintptr

	// rawRect is the GetWindowRect bounds of the window. PrintWindow renders
	// into a DC sized to these dimensions, so all offset arithmetic is done
	// in this coordinate space, not the DWM extended frame space.
	rawX   int
	rawY   int
	rawW   int
	rawH   int

	// Client rect (content area only, excluding title bar and borders)
	clientX      int
	clientY      int
	clientWidth  int
	clientHeight int

	outWidth  int
	outHeight int
	fps       int
	drawMouse bool

	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}

	mu   sync.Mutex
	conn net.Conn

	closeOnce sync.Once
	frames    atomic.Uint64
}

// NewWindowFrameSource starts listening and pumping frames. maxResolution is
// the configured output size (for example "1920x1080"); the capture is scaled
// down to fit it before it is written to the socket, which keeps a 4K window
// from pushing gigabytes per second through the CPU.
func NewWindowFrameSource(ctx context.Context, window Window, maxResolution string, fps int, drawMouse bool) (*WindowFrameSource, error) {
	if window.Handle == 0 {
		return nil, errors.New("window has no handle")
	}
	if window.Rect.Empty() {
		return nil, errors.New("window has empty bounds")
	}

	// RawRect is the actual GetWindowRect size that PrintWindow renders into.
	// If not populated (older code path or non-Windows build) fall back to Rect.
	rawRect := window.RawRect
	if rawRect.Empty() {
		rawRect = window.Rect
	}

	if rawRect.Width > 16384 || rawRect.Height > 16384 {
		return nil, errors.New("window dimensions exceed the safe limit")
	}
	if int64(rawRect.Width)*int64(rawRect.Height) > 40_000_000 {
		return nil, errors.New("window area exceeds the safe limit")
	}

	// Use client rect for output dimensions (excludes title bar and borders)
	clientRect := window.ClientRect
	if clientRect.Empty() {
		// Fall back to frame rect if client rect is not available
		clientRect = window.Rect
	}

	maxWidth, maxHeight := parseMaxResolution(maxResolution)
	width, height := fitPreview(clientRect.Width, clientRect.Height, maxWidth, maxHeight)
	// H.264 4:2:0 needs even dimensions. Rounding down is enough; the
	// PrintWindow source is stretched to this exact size below.
	width &^= 1
	height &^= 1
	if width < 2 || height < 2 {
		return nil, errors.New("window is too small to stream")
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open window capture socket: %w", err)
	}

	childCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	captureFPS := fpsOrDefault(fps)
	// PrintWindow plus a full-size DIB copy is much heavier than DXGI
	// duplication. On a 1080p-or-larger window, 60 fps costs roughly twice the
	// CPU and memory bandwidth of 30 fps for very little visible benefit, so
	// the frame source caps itself there. Small windows can keep 60.
	if width*height > 1280*720 && captureFPS > 30 {
		captureFPS = 30
	}
	s := &WindowFrameSource{
		hwnd:         window.Handle,
		rawX:         rawRect.X,
		rawY:         rawRect.Y,
		rawW:         rawRect.Width,
		rawH:         rawRect.Height,
		clientX:      clientRect.X,
		clientY:      clientRect.Y,
		clientWidth:  clientRect.Width,
		clientHeight: clientRect.Height,
		outWidth:     width,
		outHeight:    height,
		fps:          captureFPS,
		drawMouse:    drawMouse,
		listener:     listener,
		ctx:          childCtx,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	go s.serve()
	return s, nil
}

// InputArgs is the ffmpeg input half of the command line. The framerate is a
// hint; wallclock timestamps make late PrintWindow frames stay in real time
// instead of speeding the video up.
func (s *WindowFrameSource) InputArgs() []string {
	addr := s.listener.Addr().(*net.TCPAddr)
	return []string{
		"-f", "rawvideo",
		"-pixel_format", "bgra",
		"-video_size", fmt.Sprintf("%dx%d", s.outWidth, s.outHeight),
		"-framerate", strconv.Itoa(s.fps),
		"-use_wallclock_as_timestamps", "1",
		"-i", fmt.Sprintf("tcp://127.0.0.1:%d", addr.Port),
	}
}

// Close stops the pump and unblocks a waiting ffmpeg connection.
func (s *WindowFrameSource) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.listener.Close()
		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.mu.Unlock()
	})
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (s *WindowFrameSource) serve() {
	defer close(s.done)
	defer s.cancel()

	// ffmpeg connects immediately after it starts, but startup can take a
	// moment on a busy machine. A stale listener must not linger forever if
	// ffmpeg failed before opening the input.
	_ = s.listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second))
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
	}()

	grabber, err := newWindowGrabber(s.hwnd, s.rawX, s.rawY, s.rawW, s.rawH, s.clientX, s.clientY, s.clientWidth, s.clientHeight, s.outWidth, s.outHeight, s.drawMouse)
	if err != nil {
		return
	}
	defer grabber.close()

	interval := time.Second / time.Duration(s.fps)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}

		// A closed window is the natural end of this capture. ffmpeg then
		// sees EOF and the streamer reports the session as stopped.
		if r, _, _ := procIsWindow.Call(s.hwnd); r == 0 {
			return
		}
		frame, err := grabber.grab()
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= 10 {
				return
			}
			continue
		}
		consecutiveFailures = 0
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(frame); err != nil {
			return
		}
		s.frames.Add(1)
	}
}

// windowGrabber owns the two device contexts and DIB sections used for one
// capture. Reusing them avoids allocating a multi-megabyte bitmap every frame.
type windowGrabber struct {
	hwnd uintptr

	// rawX/Y/rawW/rawH are the GetWindowRect coordinates. PrintWindow renders
	// into a DC of rawW×rawH pixels and uses rawX/Y as the on-screen origin,
	// so all client-area offset arithmetic is relative to these values.
	rawX int
	rawY int
	rawW int
	rawH int

	// Client rect (content area only, excluding title bar and borders)
	clientX      int
	clientY      int
	clientWidth  int
	clientHeight int

	outWidth  int
	outHeight int
	drawMouse bool

	screenDC  uintptr
	srcDC     uintptr
	srcBitmap uintptr
	srcBits   unsafe.Pointer
	oldSrc    uintptr

	dstDC     uintptr
	dstBitmap uintptr
	dstBits   unsafe.Pointer
	oldDst    uintptr

	frame []byte
}

func newWindowGrabber(hwnd uintptr, rawX, rawY, rawW, rawH, clientX, clientY, clientWidth, clientHeight, outWidth, outHeight int, drawMouse bool) (*windowGrabber, error) {
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, errors.New("GetDC failed for window capture")
	}

	g := &windowGrabber{
		hwnd:         hwnd,
		rawX:         rawX,
		rawY:         rawY,
		rawW:         rawW,
		rawH:         rawH,
		clientX:      clientX,
		clientY:      clientY,
		clientWidth:  clientWidth,
		clientHeight: clientHeight,
		outWidth:     outWidth,
		outHeight:    outHeight,
		drawMouse:    drawMouse,
		screenDC:     screenDC,
		frame:        make([]byte, outWidth*outHeight*4),
	}
	cleanup := func() {
		if g.oldSrc != 0 && g.srcDC != 0 {
			procSelectObject.Call(g.srcDC, g.oldSrc)
		}
		if g.srcBitmap != 0 {
			procDeleteObject.Call(g.srcBitmap)
		}
		if g.srcDC != 0 {
			procDeleteDC.Call(g.srcDC)
		}
		if g.oldDst != 0 && g.dstDC != 0 {
			procSelectObject.Call(g.dstDC, g.oldDst)
		}
		if g.dstBitmap != 0 {
			procDeleteObject.Call(g.dstBitmap)
		}
		if g.dstDC != 0 {
			procDeleteDC.Call(g.dstDC)
		}
		if g.screenDC != 0 {
			procReleaseDC.Call(0, g.screenDC)
		}
	}

	var err error
	// srcDC is sized to rawW×rawH: PrintWindow fills a DC of exactly those
	// dimensions (it uses the window's actual GetWindowRect size, not the
	// DWM-trimmed extended frame). Using a smaller DIB here would cause
	// PrintWindow to render partially outside the buffer, producing grey
	// borders and cut-off content.
	g.srcDC, g.srcBitmap, g.srcBits, g.oldSrc, err = createDIBSection(g.screenDC, rawW, rawH)
	if err != nil {
		cleanup()
		return nil, err
	}
	g.dstDC, g.dstBitmap, g.dstBits, g.oldDst, err = createDIBSection(g.screenDC, outWidth, outHeight)
	if err != nil {
		cleanup()
		return nil, err
	}
	procSetStretchBltMode.Call(g.dstDC, halftone)
	return g, nil
}

func createDIBSection(screenDC uintptr, width, height int) (dc, bitmap uintptr, bits unsafe.Pointer, old uintptr, err error) {
	dc, _, _ = procCreateCompatibleDC.Call(screenDC)
	if dc == 0 {
		err = errors.New("CreateCompatibleDC failed for window capture")
		return
	}
	info := streamBitmapInfo{Header: streamBitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(streamBitmapInfoHeader{})),
		Width:       int32(width),
		Height:      int32(-height),
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
		SizeImage:   uint32(width * height * 4),
	}}
	bitmap, _, _ = procCreateDIBSection.Call(
		screenDC,
		uintptr(unsafe.Pointer(&info)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0,
		0,
	)
	if bitmap == 0 {
		err = errors.New("CreateDIBSection failed for window capture")
		return
	}
	old, _, _ = procSelectObject.Call(dc, bitmap)
	if old == 0 {
		err = errors.New("SelectObject failed for window capture")
	}
	return
}

func (g *windowGrabber) grab() ([]byte, error) {
	// PW_RENDERFULLCONTENT (2) asks DWM to render the window into our DC,
	// including GPU-composited content that a plain BitBlt cannot see.
	result, _, _ := procPrintWindow.Call(g.hwnd, g.srcDC, pwRenderFull)
	if result == 0 {
		// Some older windows ignore the flag but still support the legacy
		// path. Try it before declaring the window uncapturable.
		result, _, _ = procPrintWindow.Call(g.hwnd, g.srcDC, 0)
	}
	if result == 0 {
		// A window owned by an elevated process cannot be printed into a
		// normal process's DC. BitBlt from its DC is black for GPU windows,
		// but it is better than freezing on the last frame.
		windowDC, _, _ := procGetWindowDC.Call(g.hwnd)
		if windowDC == 0 {
			return nil, errors.New("PrintWindow failed")
		}
		ok, _, _ := procBitBlt.Call(g.srcDC, 0, 0, uintptr(g.rawW), uintptr(g.rawH), windowDC, 0, 0, srccopy|captureLayered)
		procReleaseDC.Call(g.hwnd, windowDC)
		if ok == 0 {
			return nil, errors.New("PrintWindow failed")
		}
	}
	procGdiFlush.Call()

	// Calculate the client area offset within the raw window DIB.
	// clientX/Y are screen coordinates; rawX/Y is the GetWindowRect origin
	// that PrintWindow uses. The difference is the pixel offset within the
	// srcDC where the client area starts.
	clientOffsetX := g.clientX - g.rawX
	clientOffsetY := g.clientY - g.rawY

	// Use client dimensions for source if available, otherwise fall back to raw
	srcW := g.clientWidth
	srcH := g.clientHeight
	if srcW <= 0 || srcH <= 0 {
		srcW = g.rawW
		srcH = g.rawH
		clientOffsetX = 0
		clientOffsetY = 0
	}

	ok, _, _ := procStretchBlt.Call(
		g.dstDC, 0, 0, uintptr(g.outWidth), uintptr(g.outHeight),
		g.srcDC, uintptr(clientOffsetX), uintptr(clientOffsetY), uintptr(srcW), uintptr(srcH),
		srccopy,
	)
	if ok == 0 {
		return nil, errors.New("StretchBlt failed for window capture")
	}
	procGdiFlush.Call()

	// PrintWindow renders the window's own pixels but never the mouse pointer.
	// Draw the system cursor on top of the scaled frame so window streams show
	// it just like monitor capture does.
	g.drawCursor()

	// Copy out of the DIB. The DIB is top-down (negative height), so the
	// bytes are already in the order ffmpeg expects from a rawvideo input.
	src := unsafe.Slice((*byte)(g.dstBits), len(g.frame))
	copy(g.frame, src)
	return g.frame, nil
}

// drawCursor paints the current system cursor onto the scaled output DC.
//
// PrintWindow renders the window's own pixels but never the pointer. GetCursorInfo
// returns screen coordinates, so the window's screen origin is subtracted before
// scaling. The hotspot, not the icon's top-left corner, is what lines up with
// the pointer position.
//
// Since we're capturing only the client area (excluding title bar and borders),
// the cursor position must be calculated relative to the client area origin.
func (g *windowGrabber) drawCursor() {
	if !g.drawMouse {
		return
	}
	info := cursorInfo{CbSize: uint32(unsafe.Sizeof(cursorInfo{}))}
	if ok, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return
	}
	if info.Flags&cursorShowing == 0 || info.HCursor == 0 {
		return
	}

	// The window may have moved since the frame source started. Re-read its
	// raw GetWindowRect so the pointer stays aligned while the user drags it.
	if rawRect, err := windowGetRect(g.hwnd); err == nil && !rawRect.Empty() {
		g.rawX, g.rawY = rawRect.X, rawRect.Y
		// Also update client rect if available
		if clientRect, err := windowClientRect(g.hwnd); err == nil && !clientRect.Empty() {
			g.clientX, g.clientY = clientRect.X, clientRect.Y
			g.clientWidth, g.clientHeight = clientRect.Width, clientRect.Height
		}
	}

	icon := winIconInfo{}
	if ok, _, _ := procGetIconInfo.Call(info.HCursor, uintptr(unsafe.Pointer(&icon))); ok == 0 {
		return
	}
	defer func() {
		// GetIconInfo creates these bitmaps; GDI does not own them for us.
		if icon.Mask != 0 {
			procDeleteObject.Call(uintptr(icon.Mask))
		}
		if icon.Color != 0 {
			procDeleteObject.Call(uintptr(icon.Color))
		}
	}()

	cursorWidth, cursorHeight := cursorBitmapSize(icon)

	srcW := g.clientWidth
	srcH := g.clientHeight
	if srcW <= 0 || srcH <= 0 {
		srcW = g.rawW
		srcH = g.rawH
	}

	x, y, width, height, ok := cursorDrawRect(
		int(info.PtScreenPos.X)-g.clientX,
		int(info.PtScreenPos.Y)-g.clientY,
		int(icon.XHotspot),
		int(icon.YHotspot),
		cursorWidth,
		cursorHeight,
		srcW,
		srcH,
		g.outWidth,
		g.outHeight,
	)
	if !ok {
		return
	}
	procDrawIconEx.Call(
		g.dstDC,
		uintptr(x),
		uintptr(y),
		info.HCursor,
		uintptr(width),
		uintptr(height),
		0,
		0,
		diNormal,
	)
}

func cursorBitmapSize(icon winIconInfo) (int, int) {
	var bitmap winBitmap
	if icon.Color != 0 {
		if ok, _, _ := procGetObjectW.Call(uintptr(icon.Color), unsafe.Sizeof(bitmap), uintptr(unsafe.Pointer(&bitmap))); ok != 0 {
			return int(bitmap.Width), int(bitmap.Height)
		}
	}
	if icon.Mask != 0 {
		if ok, _, _ := procGetObjectW.Call(uintptr(icon.Mask), unsafe.Sizeof(bitmap), uintptr(unsafe.Pointer(&bitmap))); ok != 0 {
			// A monochrome cursor stores the AND and XOR masks stacked, so
			// the bitmap is twice as tall as the cursor.
			return int(bitmap.Width), int(bitmap.Height) / 2
		}
	}
	return 0, 0
}

// cursorDrawRect maps the cursor's screen position into the scaled frame. It
// returns false when the hotspot is outside the window, which is the normal
// case when the pointer is over another window.
func cursorDrawRect(relX, relY, hotspotX, hotspotY, cursorWidth, cursorHeight, srcWidth, srcHeight, outWidth, outHeight int) (x, y, width, height int, ok bool) {
	if srcWidth <= 0 || srcHeight <= 0 || outWidth <= 0 || outHeight <= 0 {
		return 0, 0, 0, 0, false
	}
	if relX < 0 || relY < 0 || relX >= srcWidth || relY >= srcHeight {
		return 0, 0, 0, 0, false
	}
	if cursorWidth <= 0 || cursorHeight <= 0 {
		return 0, 0, 0, 0, false
	}
	scaleX := float64(outWidth) / float64(srcWidth)
	scaleY := float64(outHeight) / float64(srcHeight)
	x = int(math.Round(float64(relX-hotspotX) * scaleX))
	y = int(math.Round(float64(relY-hotspotY) * scaleY))
	width = int(math.Round(float64(cursorWidth) * scaleX))
	height = int(math.Round(float64(cursorHeight) * scaleY))
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return x, y, width, height, true
}

func (g *windowGrabber) close() {
	if g == nil {
		return
	}
	if g.oldSrc != 0 && g.srcDC != 0 {
		procSelectObject.Call(g.srcDC, g.oldSrc)
	}
	if g.srcBitmap != 0 {
		procDeleteObject.Call(g.srcBitmap)
	}
	if g.srcDC != 0 {
		procDeleteDC.Call(g.srcDC)
	}
	if g.oldDst != 0 && g.dstDC != 0 {
		procSelectObject.Call(g.dstDC, g.oldDst)
	}
	if g.dstBitmap != 0 {
		procDeleteObject.Call(g.dstBitmap)
	}
	if g.dstDC != 0 {
		procDeleteDC.Call(g.dstDC)
	}
	if g.screenDC != 0 {
		procReleaseDC.Call(0, g.screenDC)
	}
}

func parseMaxResolution(value string) (int, int) {
	if w, h, ok := parseTwoInts(value); ok && w >= 2 && h >= 2 {
		return w, h
	}
	return 1920, 1080
}

func parseTwoInts(value string) (int, int, bool) {
	var w, h int
	if _, err := fmt.Sscanf(value, "%dx%d", &w, &h); err != nil {
		return 0, 0, false
	}
	return w, h, true
}

func fpsOrDefault(fps int) int {
	if fps <= 0 {
		return 30
	}
	return fps
}

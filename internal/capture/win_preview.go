//go:build windows

package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	gdi32                      = windows.NewLazySystemDLL("gdi32.dll")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procStretchBlt             = gdi32.NewProc("StretchBlt")
	procGetDIBits              = gdi32.NewProc("GetDIBits")
	procSetStretchBltMode      = gdi32.NewProc("SetStretchBltMode")
	procPrintWindow            = user32.NewProc("PrintWindow")
)

const (
	srccopy        = 0x00CC0020
	captureLayered = 0x40000000
	biRGB          = 0
	dibRGBColors   = 0
	pwRenderFull   = 2
	halftone       = 4
)

type previewBitmapInfo struct {
	Header previewBitmapInfoHeader
	Colors [1]uint32
}

type previewBitmapInfoHeader struct {
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

// Preview captures a small JPEG of a selected screen or window for the picker.
func Preview(ctx context.Context, target Target, maxWidth, maxHeight int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxWidth < 1 || maxHeight < 1 || maxWidth > 1024 || maxHeight > 1024 {
		return nil, errors.New("preview dimensions must be between 1 and 1024 pixels")
	}
	source := target.Monitor.Bounds
	if target.Kind == KindWindow {
		source = target.Window.Rect
	}
	if source.Empty() {
		return nil, errors.New("preview target has empty bounds")
	}
	if source.Width > 16384 || source.Height > 16384 {
		return nil, errors.New("preview target dimensions exceed the safe limit")
	}
	if int64(source.Width)*int64(source.Height) > 40_000_000 {
		return nil, errors.New("preview target area exceeds the safe limit")
	}
	width, height := fitPreview(source.Width, source.Height, maxWidth, maxHeight)

	screenDC, _, _ := user32.NewProc("GetDC").Call(0)
	if screenDC == 0 {
		return nil, errors.New("GetDC failed for the desktop")
	}
	defer user32.NewProc("ReleaseDC").Call(0, screenDC)

	sourceDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if sourceDC == 0 {
		return nil, errors.New("CreateCompatibleDC failed for preview source")
	}
	defer procDeleteDC.Call(sourceDC)
	sourceBitmap, _, _ := procCreateCompatibleBitmap.Call(screenDC, uintptr(source.Width), uintptr(source.Height))
	if sourceBitmap == 0 {
		return nil, errors.New("CreateCompatibleBitmap failed for preview source")
	}
	defer procDeleteObject.Call(sourceBitmap)
	oldSource, _, _ := procSelectObject.Call(sourceDC, sourceBitmap)
	if oldSource == 0 {
		return nil, errors.New("SelectObject failed for preview source")
	}
	defer procSelectObject.Call(sourceDC, oldSource)

	captured := false
	if target.Kind == KindWindow {
		result, _, _ := procPrintWindow.Call(uintptr(target.Window.Handle), sourceDC, pwRenderFull)
		captured = result != 0
	}
	if !captured {
		result, _, err := procBitBlt.Call(
			sourceDC, 0, 0, uintptr(source.Width), uintptr(source.Height),
			screenDC, uintptr(int64(source.X)), uintptr(int64(source.Y)), srccopy|captureLayered,
		)
		if result == 0 {
			return nil, fmt.Errorf("BitBlt failed while capturing preview: %w", err)
		}
	}

	previewDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if previewDC == 0 {
		return nil, errors.New("CreateCompatibleDC failed for preview")
	}
	defer procDeleteDC.Call(previewDC)
	previewBitmap, _, _ := procCreateCompatibleBitmap.Call(screenDC, uintptr(width), uintptr(height))
	if previewBitmap == 0 {
		return nil, errors.New("CreateCompatibleBitmap failed for preview")
	}
	defer procDeleteObject.Call(previewBitmap)
	oldPreview, _, _ := procSelectObject.Call(previewDC, previewBitmap)
	if oldPreview == 0 {
		return nil, errors.New("SelectObject failed for preview")
	}
	selectedPreview := true
	defer func() {
		if selectedPreview {
			procSelectObject.Call(previewDC, oldPreview)
		}
	}()
	procSetStretchBltMode.Call(previewDC, halftone)
	result, _, _ := procStretchBlt.Call(
		previewDC, 0, 0, uintptr(width), uintptr(height),
		sourceDC, 0, 0, uintptr(source.Width), uintptr(source.Height),
		srccopy,
	)
	if result == 0 {
		return nil, errors.New("StretchBlt failed while scaling preview")
	}
	procSelectObject.Call(previewDC, oldPreview)
	selectedPreview = false
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pixels := make([]byte, width*height*4)
	info := previewBitmapInfo{Header: previewBitmapInfoHeader{
		Size: uint32(unsafe.Sizeof(previewBitmapInfoHeader{})), Width: int32(width),
		Height: -int32(height), Planes: 1, BitCount: 32, Compression: biRGB,
		SizeImage: uint32(len(pixels)),
	}}
	lines, _, _ := procGetDIBits.Call(
		previewDC, previewBitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&pixels[0])),
		uintptr(unsafe.Pointer(&info)), dibRGBColors,
	)
	if lines != uintptr(height) {
		return nil, fmt.Errorf("GetDIBits returned %d scanlines, want %d", lines, height)
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			offset := (y*width + x) * 4
			img.SetRGBA(x, y, color.RGBA{
				R: pixels[offset+2], G: pixels[offset+1], B: pixels[offset], A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 68}); err != nil {
		return nil, fmt.Errorf("encode picker preview: %w", err)
	}
	return encoded.Bytes(), nil
}

func fitPreview(width, height, maxWidth, maxHeight int) (int, int) {
	scale := math.Min(float64(maxWidth)/float64(width), float64(maxHeight)/float64(height))
	if scale > 1 {
		scale = 1
	}
	return max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale))
}

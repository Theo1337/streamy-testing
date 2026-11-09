//go:build windows

package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Wails window gets its title bar and taskbar icon at runtime. Without this
// the settings app would show the generic Go executable icon while the tray
// already uses the real Streamy icon.
//
//go:embed assets/icon_idle.ico
var windowIconICO []byte

var (
	user32Icon      = windows.NewLazySystemDLL("user32.dll")
	procFindWindowW = user32Icon.NewProc("FindWindowW")
	procLoadImageW  = user32Icon.NewProc("LoadImageW")
	procSendMessage = user32Icon.NewProc("SendMessageW")
)

const (
	wmSetIcon      = 0x0080
	imageIcon      = 1
	lrLoadFromFile = 0x0010
	iconBig        = 1
	iconSmall      = 0
)

func setWindowIcon() {
	path := filepath.Join(os.TempDir(), "streamy-ui-icon.ico")
	if err := os.WriteFile(path, windowIconICO, 0o600); err != nil {
		return
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}

	// Wails creates the webview window after OnStartup returns, so poll briefly
	// for it instead of racing the framework.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, title := range []string{"Streamy settings", "Streamy — Share your screen", "Streamy — Updating"} {
			titlePtr, err := windows.UTF16PtrFromString(title)
			if err != nil {
				continue
			}
			hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
			if hwnd == 0 {
				continue
			}
			icon, _, _ := procLoadImageW.Call(
				0, uintptr(unsafe.Pointer(pathPtr)), imageIcon, 0, 0, lrLoadFromFile,
			)
			if icon != 0 {
				procSendMessage.Call(hwnd, wmSetIcon, iconBig, icon)
				procSendMessage.Call(hwnd, wmSetIcon, iconSmall, icon)
			}
			return
		}
		time.Sleep(120 * time.Millisecond)
	}
}

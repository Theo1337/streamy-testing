package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed frontend/dist
var frontend embed.FS

func main() {
	address := flag.String("ipc-address", "", "authenticated tray IPC address")
	token := flag.String("ipc-token", "", "one-time tray IPC token")
	view := flag.String("view", "settings", "window to show: settings, share, or update")
	flag.Parse()
	if *address == "" || *token == "" {
		log.Fatal("Streamy UI must be launched by the tray process")
	}
	if *view != "share" && *view != "update" {
		*view = "settings"
	}
	assets, err := fs.Sub(frontend, "frontend/dist")
	if err != nil {
		log.Fatal(fmt.Errorf("load embedded frontend: %w", err))
	}
	app := NewSettingsApp(*address, *token, *view)
	title := "Streamy settings"
	width := 980
	height := 760
	disableResize := false
	if *view == "share" {
		title = "Streamy — Share your screen"
		width = 1100
		height = 780
	} else if *view == "update" {
		title = "Streamy — Updating"
		width = 480
		height = 380
		// The update view is a small fixed progress dialog. Allowing it to be
		// resized smaller than its content was what produced a scrollbar.
		disableResize = true
	}
	err = wails.Run(&options.App{
		Title:            title,
		Width:            width,
		Height:           height,
		MinWidth:         400,
		MinHeight:        200,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 17, G: 24, B: 39, A: 1},
		WindowStartState: options.Normal,
		DisableResize:    disableResize,
		Frameless:        false,
		DragAndDrop:      nil,
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
	})
	if err != nil {
		log.Fatal(fmt.Errorf("run settings window: %w", err))
	}
}

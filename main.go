package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"

	"logviewer/internal/service"
)

// Wails uses Go's `embed` package to embed the frontend files into the binary.
// Any files in the frontend/dist folder will be embedded into the binary and
// made available to the frontend.
// See https://pkg.go.dev/embed for more information.

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	// 注册自定义事件：binding 生成器据此生成强类型 JS/TS 事件 API。
	application.RegisterEvent[service.IndexProgressEvent]("indexProgress")
	application.RegisterEvent[service.SearchProgressEvent]("searchProgress")
}

// main 是应用入口：注册 LogService、配置窗口并运行。
func main() {
	app := application.New(application.Options{
		Name:        "logviewer",
		Description: "结构化日志（JSONL）查看与检索工具",
		Services: []application.Service{
			application.NewService(service.NewLogService()),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "结构化日志查看器",
		Width:  1280,
		Height: 800,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(20, 22, 28),
		URL:              "/",
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

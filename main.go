package main

import (
	"log"
	"os"

	"gioui.org/app"

	"thermalapp/camera"
	"thermalapp/ui"
)

func main() {
	cam := camera.NewP3Camera()

	log.Println("Connecting to P3 camera...")
	if err := cam.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer cam.Close()

	log.Println("Initializing...")
	info, err := cam.Init()
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	log.Printf("Camera: %s  FW: %s  HW: %s  Serial: %s",
		info.Model, info.FWVersion, info.HWVersion, info.Serial)

	log.Println("Starting stream...")
	if err := cam.StartStreaming(); err != nil {
		log.Fatalf("start streaming: %v", err)
	}

	a := ui.NewApp(cam)

	// USB reader goroutine
	go func() {
		for {
			frame, err := cam.ReadFrame()
			if err != nil {
				log.Printf("read frame: %v", err)
				continue
			}
			a.UpdateFrame(frame)
		}
	}()

	// Gio main loop (must run on main thread)
	go func() {
		if err := a.Run(); err != nil {
			log.Printf("ui: %v", err)
		}
		os.Exit(0)
	}()

	app.Main()
}

package main

import (
	"flag"
	"log"
	"os"

	"gioui.org/app"

	"thermalapp/camera"
	"thermalapp/recording"
	"thermalapp/ui"
)

func main() {
	playFile := flag.String("play", "", "play back a .tha recording file instead of connecting to camera")
	flag.Parse()

	if *playFile != "" {
		runPlayback(*playFile)
		return
	}

	runLive()
}

func runLive() {
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

func runPlayback(filename string) {
	player, err := recording.NewPlayer(filename)
	if err != nil {
		log.Fatalf("open recording: %v", err)
	}
	defer player.Close()

	h := player.Header()
	log.Printf("Playing %s: %dx%d, %d frames", filename, h.Width, h.Height, h.FrameCount)

	a := ui.NewApp(player)
	a.SetPlayer(player)

	// Frame reader goroutine (respects original timing)
	go func() {
		for {
			frame, err := player.ReadFrame()
			if err != nil {
				// "paused" is not a real error
				continue
			}
			a.UpdateFrame(frame)
		}
	}()

	go func() {
		if err := a.Run(); err != nil {
			log.Printf("ui: %v", err)
		}
		os.Exit(0)
	}()

	app.Main()
}

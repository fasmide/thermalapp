package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"gioui.org/app"

	"thermalapp/camera"
	"thermalapp/recording"
	"thermalapp/ui"
)

const (
	defaultBufSizeMB  = 500         // default frame buffer size in megabytes
	bytesPerMB        = 1024 * 1024 // bytes per megabyte
	metadataRowWidth  = 256         // number of columns per metadata row (matches p3SensorW)
	metadataPrintCols = 16          // values printed per line in metadata dumps
)

func main() {
	playFile := flag.String("play", "", "play back a .tha recording file instead of connecting to camera")
	bufSizeMB := flag.Int("bufsize", defaultBufSizeMB, "frame buffer size in megabytes for temperature history")
	revMeta := flag.Bool("rev_meta", false, "dump frame metadata to terminal for reverse engineering")
	revMeta2 := flag.Bool("rev_meta2", false, "compact metadata monitor: track key registers, one line per frame")
	flag.Parse()

	bufBytes := int64(*bufSizeMB) * bytesPerMB

	if *playFile != "" {
		runPlayback(*playFile, bufBytes)

		return
	}

	if *revMeta2 {
		runRevMeta2()

		return
	}

	if *revMeta {
		runRevMeta()

		return
	}

	runLive(bufBytes)
}

func runLive(bufBytes int64) {
	cam := camera.NewP3Camera()

	log.Println("Connecting to P3 camera...")
	if err := cam.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}

	log.Println("Initializing...")
	info, err := cam.Init()
	if err != nil {
		cam.Close()
		log.Fatalf("init: %v", err)
	}
	log.Printf("Camera: %s  FW: %s  HW: %s  Serial: %s",
		info.Model, info.FWVersion, info.HWVersion, info.Serial)

	log.Println("Starting stream...")
	if err := cam.StartStreaming(); err != nil {
		cam.Close()
		log.Fatalf("start streaming: %v", err)
	}
	defer cam.Close()

	uiApp := ui.NewApp(cam, bufBytes)

	// USB reader goroutine
	go func() {
		for {
			frame, err := cam.ReadFrame()
			if err != nil {
				log.Printf("read frame: %v", err)

				continue
			}
			uiApp.UpdateFrame(frame)
		}
	}()

	// Gio main loop (must run on main thread)
	go func() {
		if err := uiApp.Run(); err != nil {
			log.Printf("ui: %v", err)
		}
		os.Exit(0)
	}()

	app.Main()
}

func runPlayback(filename string, bufBytes int64) {
	player, err := recording.NewPlayer(filename)
	if err != nil {
		log.Fatalf("open recording: %v", err)
	}
	defer player.Close()

	h := player.Header()
	log.Printf("Playing %s: %dx%d, %d frames", filename, h.Width, h.Height, h.FrameCount)

	uiApp := ui.NewApp(player, bufBytes)
	uiApp.SetPlayer(player)

	// Frame reader goroutine (respects original timing)
	go func() {
		for {
			frame, err := player.ReadFrame()
			if err != nil {
				// "paused" is not a real error
				continue
			}
			uiApp.UpdateFrame(frame)
		}
	}()

	go func() {
		if err := uiApp.Run(); err != nil {
			log.Printf("ui: %v", err)
		}
		os.Exit(0)
	}()

	app.Main()
}

func runRevMeta() {
	cam := camera.NewP3Camera()

	log.Println("Connecting to P3 camera...")
	if err := cam.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}

	log.Println("Initializing...")
	if _, err := cam.Init(); err != nil {
		cam.Close()
		log.Fatalf("init: %v", err)
	}

	log.Println("Starting stream...")
	if err := cam.StartStreaming(); err != nil {
		cam.Close()
		log.Fatalf("start streaming: %v", err)
	}
	defer cam.Close()

	log.Println("Streaming metadata (Ctrl-C to stop)...")
	log.Println("Format: [row col / flat_idx] old -> new (hex). Only changed values shown.")
	log.Println("Row 0: metadata[0..255], Row 1: metadata[256..511]")

	var prev []uint16
	frameNum := 0

	for {
		frame, err := cam.ReadFrame()
		if err != nil {
			log.Printf("read frame: %v", err)

			continue
		}
		frameNum++

		meta := frame.Metadata
		if prev == nil {
			// First frame: print all values
			fmt.Printf("=== Frame %d (initial) ===\n", frameNum)
			fmt.Println("Row 0:")
			for i := 0; i < metadataRowWidth && i < len(meta); i++ {
				fmt.Printf("  [%3d]=%04x", i, meta[i])
				if (i+1)%metadataPrintCols == 0 {
					fmt.Println()
				}
			}
			fmt.Println("Row 1:")
			for i := metadataRowWidth; i < 2*metadataRowWidth && i < len(meta); i++ {
				fmt.Printf("  [%3d]=%04x", i, meta[i])
				if (i-metadataRowWidth+1)%metadataPrintCols == 0 {
					fmt.Println()
				}
			}
			prev = make([]uint16, len(meta))
			copy(prev, meta)

			continue
		}

		// Print only changes
		changed := 0
		for i := range meta {
			if i < len(prev) && meta[i] != prev[i] {
				changed++
			}
		}
		if changed > 0 {
			fmt.Printf("=== Frame %d (%d changes) ===\n", frameNum, changed)
			for i := range meta {
				if i < len(prev) && meta[i] != prev[i] {
					row := i / metadataRowWidth
					col := i % metadataRowWidth
					fmt.Printf("  [r%d c%3d / %3d] %04x -> %04x\n", row, col, i, prev[i], meta[i])
				}
			}
			copy(prev, meta)
		}
	}
}

func runRevMeta2() {
	cam := camera.NewP3Camera()

	log.Println("Connecting to P3 camera...")
	if err := cam.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}

	log.Println("Initializing...")
	if _, err := cam.Init(); err != nil {
		cam.Close()
		log.Fatalf("init: %v", err)
	}

	log.Println("Starting stream...")
	if err := cam.StartStreaming(); err != nil {
		cam.Close()
		log.Fatalf("start streaming: %v", err)
	}
	defer cam.Close()

	// Registers of interest (row 0 only)
	type reg struct {
		idx  int
		name string
	}
	regs := []reg{
		{64, "frameCnt"},
		{65, "r65"},
		{68, "r68"},
		{70, "r70"},
		{71, "r71"},
		{72, "countdown1"},
		{73, "r73"},
		{74, "r74"},
		{75, "countdown2"},
		{165, "temp1"},
		{166, "r166"},
		{167, "r167"},
		{168, "temp2"},
		{169, "r169"},
		{170, "r170"},
		{173, "temp3"},
		{174, "temp4"},
		{175, "temp5"},
		{176, "temp6"},
		{179, "r179"},
		{180, "counter2"},
	}

	// Print header
	fmt.Printf("%8s", "frame")
	for _, r := range regs {
		fmt.Printf(" %12s", r.name)
	}
	fmt.Println()

	var prev []uint16
	frameNum := 0

	for {
		frame, err := cam.ReadFrame()
		if err != nil {
			log.Printf("read frame: %v", err)

			continue
		}
		frameNum++
		meta := frame.Metadata

		// Check if any tracked register changed
		anyChange := prev == nil
		if !anyChange {
			for _, r := range regs {
				if r.idx < len(meta) && r.idx < len(prev) && meta[r.idx] != prev[r.idx] {
					anyChange = true

					break
				}
			}
		}

		// Also check for ANY change outside tracked registers (flag it)
		extraChanges := 0
		if prev != nil {
			for i := range meta {
				if i < len(prev) && meta[i] != prev[i] {
					tracked := false
					for _, r := range regs {
						if r.idx == i {
							tracked = true

							break
						}
					}
					if !tracked {
						extraChanges++
					}
				}
			}
		}

		if anyChange || extraChanges > 0 {
			fmt.Printf("%8d", frameNum)
			for _, r := range regs {
				if r.idx < len(meta) {
					fmt.Printf(" %12s", fmt.Sprintf("%04x", meta[r.idx]))
				} else {
					fmt.Printf(" %12s", "----")
				}
			}
			if extraChanges > 0 {
				fmt.Printf("  +%d untracked", extraChanges)
			}
			fmt.Println()
		}

		if prev == nil {
			prev = make([]uint16, len(meta))
		}
		copy(prev, meta)
	}
}

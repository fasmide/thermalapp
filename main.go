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

	// Flat-field calibration defaults.
	ffcDefaultWarmup    = 10  // frames to discard before sampling
	ffcDefaultSmoothing = 100 // frames to average
	ffcDefaultOutput    = "flat_field.png"
)

func main() {
	playFile := flag.String("play", "", "play back a .tha recording file instead of connecting to camera")
	bufSizeMB := flag.Int("bufsize", defaultBufSizeMB, "frame buffer size in megabytes for temperature history")
	cameraType := flag.String("camera", "auto", "camera type: auto, p3, seek")
	ffcFile := flag.String("ffc", "", "additional flat-field correction PNG (removes sensor gradient)")
	createFFC := flag.Bool("create-ffc", false,
		"create a flat-field calibration image (point camera at uniform surface)")
	ffcOutput := flag.String("ffc-output", ffcDefaultOutput, "output filename for -create-ffc")
	ffcWarmup := flag.Int("ffc-warmup", ffcDefaultWarmup, "warmup frames to discard for -create-ffc")
	ffcSmoothing := flag.Int("ffc-smoothing", ffcDefaultSmoothing, "frames to average for -create-ffc")
	revMeta := flag.Bool("rev_meta", false, "dump frame metadata to terminal for reverse engineering (P3 only)")
	revMeta2 := flag.Bool("rev_meta2", false, "compact metadata monitor: track key registers (P3 only)")
	flag.Parse()

	bufBytes := int64(*bufSizeMB) * bytesPerMB

	if *playFile != "" {
		runPlayback(*playFile, bufBytes)

		return
	}

	if *createFFC {
		runCreateFFC(*cameraType, *ffcOutput, *ffcWarmup, *ffcSmoothing)

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

	runLive(*cameraType, *ffcFile, bufBytes)
}

// openCamera creates, connects, and initializes a camera based on the type string.
// For "auto" mode, it tries Seek first, then falls back to P3.
func openCamera(cameraType string) (camera.Camera, camera.DeviceInfo, error) {
	switch cameraType {
	case "p3":
		return connectCamera(camera.NewP3Camera())
	case "seek":
		return connectSeekCamera()
	case "auto":
		return openCameraAuto()
	default:
		return nil, camera.DeviceInfo{}, fmt.Errorf("unknown camera type: %q (use auto, p3, or seek)", cameraType)
	}
}

// connectSeekCamera creates, connects, and initializes a Seek CompactPRO camera.
func connectSeekCamera() (camera.Camera, camera.DeviceInfo, error) {
	seekCam := camera.NewSeekCamera()

	if err := seekCam.Connect(); err != nil {
		return nil, camera.DeviceInfo{}, fmt.Errorf("connect: %w", err)
	}

	info, err := seekCam.Init()
	if err != nil {
		seekCam.Close()

		return nil, camera.DeviceInfo{}, fmt.Errorf("init: %w", err)
	}

	return seekCam, info, nil
}

// openCameraAuto tries Seek first, then falls back to P3.
func openCameraAuto() (camera.Camera, camera.DeviceInfo, error) {
	log.Println("Auto-detecting camera...")

	seekCam := camera.NewSeekCamera()
	if err := seekCam.Connect(); err == nil {
		log.Println("Found Seek camera, initializing...")

		info, initErr := seekCam.Init()
		if initErr == nil {
			return seekCam, info, nil
		}

		seekCam.Close()
		log.Printf("Seek init failed: %v, trying P3...", initErr)
	} else {
		log.Printf("No Seek camera found: %v, trying P3...", err)
	}

	return connectCamera(camera.NewP3Camera())
}

// connectCamera connects and initializes a single camera.
func connectCamera(cam camera.Camera) (camera.Camera, camera.DeviceInfo, error) {
	if err := cam.Connect(); err != nil {
		return nil, camera.DeviceInfo{}, fmt.Errorf("connect: %w", err)
	}

	info, err := cam.Init()
	if err != nil {
		cam.Close()

		return nil, camera.DeviceInfo{}, fmt.Errorf("init: %w", err)
	}

	return cam, info, nil
}

func runLive(cameraType, ffcFile string, bufBytes int64) {
	cam, info, err := openCamera(cameraType)
	if err != nil {
		log.Fatalf("camera: %v", err)
	}

	log.Printf("Camera: %s  FW: %s  HW: %s  Serial: %s",
		info.Model, info.FWVersion, info.HWVersion, info.Serial)

	// Load optional additional flat-field correction.
	var ffc *camera.FlatFieldCorrector

	if ffcFile != "" {
		ffc, err = camera.LoadFlatField(ffcFile)
		if err != nil {
			cam.Close()
			log.Fatalf("load flat-field: %v", err)
		}

		log.Printf("Loaded additional flat-field correction from %s", ffcFile)
	}

	log.Println("Starting stream...")
	if err := cam.StartStreaming(); err != nil {
		cam.Close()
		log.Fatalf("start streaming: %v", err)
	}
	defer cam.Close()

	uiApp := ui.NewApp(cam, info.Model, bufBytes)

	// USB reader goroutine
	go func() {
		for {
			frame, readErr := cam.ReadFrame()
			if readErr != nil {
				log.Printf("read frame: %v", readErr)

				continue
			}

			if ffc != nil {
				if ffcErr := ffc.Apply(frame); ffcErr != nil {
					log.Printf("apply flat-field: %v", ffcErr)
				}
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

// runCreateFFC creates a flat-field calibration image by averaging corrected
// frames while the camera is pointed at a uniform surface.
func runCreateFFC(cameraType, output string, warmup, smoothing int) {
	cam, info, err := openCamera(cameraType)
	if err != nil {
		log.Fatalf("camera: %v", err)
	}

	log.Printf("Camera: %s — creating flat-field calibration image", info.Model)

	if err := cam.StartStreaming(); err != nil {
		cam.Close()
		log.Fatalf("start streaming: %v", err)
	}

	size := cam.SensorSize()
	log.Printf("Sensor: %dx%d, warming up %d frames...", size.X, size.Y, warmup)

	// Warmup: discard initial frames so the sensor stabilizes.
	for idx := range warmup {
		if _, warmErr := cam.ReadFrame(); warmErr != nil {
			log.Printf("warmup frame %d: %v", idx+1, warmErr)
		}
	}

	log.Printf("Warmup complete. Averaging %d frames (keep camera pointed at uniform surface)...", smoothing)

	acc := camera.NewFlatFieldAccumulator(size.X, size.Y)

	for acc.Count() < smoothing {
		frame, readErr := cam.ReadFrame()
		if readErr != nil {
			log.Printf("frame read: %v", readErr)

			continue
		}

		if addErr := acc.Add(frame); addErr != nil {
			log.Printf("accumulate frame: %v", addErr)

			continue
		}

		if acc.Count()%10 == 0 { //nolint:mnd // progress indicator at round numbers
			log.Printf("  collected %d / %d frames", acc.Count(), smoothing)
		}
	}

	cam.Close()

	if err := acc.Save(output); err != nil {
		log.Fatalf("save flat-field: %v", err)
	}

	log.Printf("Flat-field calibration saved to %s", output)
	log.Printf("Use with: -ffc %s", output)
}

func runPlayback(filename string, bufBytes int64) {
	player, err := recording.NewPlayer(filename)
	if err != nil {
		log.Fatalf("open recording: %v", err)
	}
	defer player.Close()

	h := player.Header()
	log.Printf("Playing %s: %dx%d, %d frames", filename, h.Width, h.Height, h.FrameCount)

	uiApp := ui.NewApp(player, "Playback", bufBytes)
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
		meta, err := cam.ReadMetadata()
		if err != nil {
			log.Printf("read metadata: %v", err)

			continue
		}
		frameNum++

		if prev == nil {
			printInitialMetadata(frameNum, meta)
			prev = make([]uint16, len(meta))
			copy(prev, meta)

			continue
		}

		printChangedMetadata(frameNum, meta, prev)
		copy(prev, meta)
	}
}

// printInitialMetadata prints all metadata values on the first frame.
func printInitialMetadata(frameNum int, meta []uint16) {
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
}

// printChangedMetadata prints only the metadata values that changed since prev.
func printChangedMetadata(frameNum int, meta, prev []uint16) {
	changed := 0

	for i := range meta {
		if i < len(prev) && meta[i] != prev[i] {
			changed++
		}
	}

	if changed == 0 {
		return
	}

	fmt.Printf("=== Frame %d (%d changes) ===\n", frameNum, changed)

	for i := range meta {
		if i < len(prev) && meta[i] != prev[i] {
			row := i / metadataRowWidth
			col := i % metadataRowWidth
			fmt.Printf("  [r%d c%3d / %3d] %04x -> %04x\n", row, col, i, prev[i], meta[i])
		}
	}
}

// metaReg describes a metadata register to track in runRevMeta2.
type metaReg struct {
	idx  int
	name string
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

	regs := []metaReg{
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
		meta, err := cam.ReadMetadata()
		if err != nil {
			log.Printf("read metadata: %v", err)

			continue
		}
		frameNum++
		anyChange := hasTrackedChange(meta, prev, regs)
		extraChanges := countExtraChanges(meta, prev, regs)

		if anyChange || extraChanges > 0 {
			printRegRow(frameNum, meta, regs, extraChanges)
		}

		if prev == nil {
			prev = make([]uint16, len(meta))
		}

		copy(prev, meta)
	}
}

// hasTrackedChange reports whether any of the tracked registers changed.
func hasTrackedChange(meta, prev []uint16, regs []metaReg) bool {
	if prev == nil {
		return true
	}

	for _, r := range regs {
		if r.idx < len(meta) && r.idx < len(prev) && meta[r.idx] != prev[r.idx] {
			return true
		}
	}

	return false
}

// countExtraChanges counts metadata changes outside the tracked registers.
func countExtraChanges(meta, prev []uint16, regs []metaReg) int {
	if prev == nil {
		return 0
	}

	count := 0

	for idx := range meta {
		if idx >= len(prev) || meta[idx] == prev[idx] {
			continue
		}

		tracked := false

		for _, r := range regs {
			if r.idx == idx {
				tracked = true

				break
			}
		}

		if !tracked {
			count++
		}
	}

	return count
}

// printRegRow prints one data row: frame number, tracked register values, and extra-change count.
func printRegRow(frameNum int, meta []uint16, regs []metaReg, extraChanges int) {
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

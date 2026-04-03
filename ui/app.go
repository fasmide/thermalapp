// Package ui implements the Gio-based thermal camera viewer.
package ui

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"thermalapp/camera"
	"thermalapp/colorize"
	"thermalapp/recording"
)

const (
	windowScaleFactor       = 3                      // pixel scale for initial window size
	windowHeightPadDp       = 80                     // extra height padding for status bars in dp
	spotCursorIdx           = 2                      // index of the cursor spot in spots slice
	firstUserSpotIdx        = 3                      // index of the first user-placed spot
	toastShortDuration      = 2 * time.Second        // duration for short toast notifications
	toastDuration           = 3 * time.Second        // duration for standard toast notifications
	backfillDebounce        = 150 * time.Millisecond // debounce delay before starting backfill
	frameBytesPerPixel      = 4                      // bytes per pixel when sizing buffer panel
	msPerFrameAt25FPS       = 40                     // milliseconds per frame at 25 fps
	nucBadgeInsetDp         = 8                      // NUC badge inset from image edge in dp
	colorbarHeightDp        = 20                     // colorbar height in dp
	colorbarLabelXOffset    = 4                      // x offset for colorbar min label in px
	colorbarMaxLabelInsetDp = 55                     // right inset for colorbar max label in dp
	lumThreshold            = 128                    // luminance threshold for contrast text color
	playbackBarHeightDp     = 28                     // playback bar height in dp
	frameCounterInsetDp     = 6                      // left inset for frame counter label in dp
	sliderHeightDp          = 12                     // slider height in dp
	sliderTrackHeightDp     = 4                      // slider track height in dp
	sliderThumbWidthDp      = 8                      // slider thumb width in dp
	degreesPerRotStep       = 90                     // degrees per rotation step
	spotHitRadiusSq         = 25                     // squared hit radius for spot selection in px
	keyHelpColumnWidthDp    = 100                    // key column width in help overlay in dp
	keyHelpPanelOffsetDp    = 20                     // help overlay offset from top-left in dp
	keyHelpTitleBottomDp    = 6                      // bottom inset below help title in dp
	keyHelpSectionTopDp     = 10                     // top padding for help sections in dp
	keyHelpExtraWidthDp     = 220                    // extra width for help overlay beyond key col in dp
	keyHelpPanelInsetDp     = 12                     // uniform inset inside help overlay in dp
	toastPaddingDp          = 10                     // padding inside toast notification in dp
	toastBottomOffsetDp     = 80                     // offset from bottom of window for toast in dp

	rotationCount      = 4  // total number of rotation steps (0°, 90°, 180°, 270°)
	selectionRingExtra = 3  // extra pixels for the selection highlight ring around a spot
	cursorLabelXOff    = 12 // cursor temp label x offset from cursor position in px
	cursorLabelYOff    = 6  // cursor temp label y offset above cursor position in px

	// Inset sizes (dp) used for labels and bars.
	labelInsetSmDp   = 1 // top/bottom inset for spot/temp labels
	labelInsetMdDp   = 3 // left/right inset for spot/temp labels; NUC badge top/bottom
	labelInsetLgDp   = 6 // left/right inset for NUC badge
	playbarInsetDp   = 2 // uniform inset for the playback bar content
	statusBarInsetDp = 4 // uniform inset for the status bar content
	playBtnInsetXDp  = 8 // left/right padding inside the play/pause button
	playBtnInsetYDp  = 2 // top/bottom padding inside the play/pause button
	frameCounterRDp  = 4 // right inset for frame counter label
	sectionTopPad0Dp = 2 // top padding for the first help section (no gap)
	sectionBotPadDp  = 2 // bottom padding for help section title rows

	// Playback navigation step for "go back one frame" (FrameIndex returns next frame, so -2 steps back).
	seekBackStep = 2

	// centerDiv is the divisor used when centering the image within the available area.
	centerDiv = 2

	// labelHCenterDiv centers a label horizontally by dividing its width in half.
	labelHCenterDiv = 2

	// spotLabelYAdj nudges the spot label upward by this many pixels above the marker center.
	spotLabelYAdj = 2

	// markerDiameterMult multiplies markerSize to get the full diameter of a spot marker.
	markerDiameterMult = 2

	// Rec.601 luminance weights for perceived brightness calculation.
	lumWeightR = 0.299 // red channel luminance weight
	lumWeightG = 0.587 // green channel luminance weight
	lumWeightB = 0.114 // blue channel luminance weight
)

// spotSnap is a thread-safe snapshot of a Spot's mutable state, used during layout.
type spotSnap struct {
	sp    *Spot
	state SpotState
}

// App holds the UI and shared state.
type App struct {
	Window    *app.Window
	theme     *material.Theme
	modelName string // camera model name for window title

	mu     sync.Mutex
	result *colorize.Result
	params colorize.Params
	cam    camera.Camera

	// Measurement spots: 0=min, 1=max, 2=cursor, 3+=user
	spots  []*Spot
	graphs map[int]*GraphWindow // index -> graph window

	// Cursor screen position (for drawing label next to pointer)
	cursorPos f32.Point

	// Image layout info (updated each frame for cursor mapping)
	imgOffsetX, imgOffsetY int
	imgScale               float32

	// Gain state
	gainMode camera.GainMode

	// EMA smoothing initialized flag
	smInited atomic.Bool

	// Rotation: 0=0°, 1=90°, 2=180°, 3=270°
	rotation int

	// Emissivity preset index (into colorize.EmissivityPresets)
	emissivityIdx int

	// Selected spot index for per-spot emissivity (-1 = none)
	selectedSpot int

	// Emissivity dropdown
	epsDropdown *EmissivityDropdown
	epsClick    widget.Clickable

	// Buffer settings panel
	bufPanel *BufferPanel
	bufClick widget.Clickable

	// Toggles
	showColorbar bool
	showHelp     bool
	showLabels   bool

	// Toast notification
	toastMsg    string
	toastExpiry time.Time

	// Tag for input events
	tag bool

	// Recording state
	lastFrame *camera.Frame // last raw frame for D-key dump
	recorder  *recording.Recorder

	// Shutter/NUC state (updated each frame)
	shutterActive bool

	// Playback state (non-nil when playing a recording)
	player *recording.Player

	// Playback bar state
	playPauseClick widget.Clickable
	sliderTag      bool // event tag for the slider area
	sliderDragging bool

	// Backfill debounce: timer fires after slider drag stops
	backfillTimer *time.Timer

	// Frame buffer for temperature history graphs (live mode)
	frameBuf *FrameBuffer

	// Playback buffer for temperature history graphs (playback mode)
	playBuf *PlaybackBuffer
}

func NewApp(cam camera.Camera, modelName string, bufSize int64) *App {
	size := cam.SensorSize()
	var win app.Window

	title := modelName
	if title == "" {
		title = "Thermal"
	}

	win.Option(
		app.Title(title),
		app.Size(unit.Dp(float32(size.X*windowScaleFactor)), unit.Dp(float32(size.Y*windowScaleFactor+windowHeightPadDp))),
	)

	return &App{
		Window:    &win,
		theme:     material.NewTheme(),
		modelName: title,
		cam:       cam,
		params: colorize.Params{
			Mode:       colorize.AGCPercentile,
			Palette:    colorize.PaletteInferno,
			Emissivity: colorize.DefaultEmissivity,
		},
		spots: []*Spot{
			NewSpot(0, SpotMin, color.NRGBA{R: 60, G: 120, B: 255, A: 230}),
			NewSpot(1, SpotMax, color.NRGBA{R: 255, G: 60, B: 60, A: 230}),
			NewSpot(spotCursorIdx, SpotCursor, color.NRGBA{R: 180, G: 180, B: 180, A: 200}),
		},
		graphs:       make(map[int]*GraphWindow),
		frameBuf:     NewFrameBuffer(size.X, size.Y, bufSize),
		selectedSpot: -1,
		epsDropdown:  NewEmissivityDropdown(),
		bufPanel:     NewBufferPanel(),
		showColorbar: true,
		showLabels:   true,
	}
}

// SetPlayer configures the app for playback mode.
func (a *App) SetPlayer(p *recording.Player) {
	a.player = p
	h := p.Header()
	a.playBuf = NewPlaybackBuffer(int(h.Width), int(h.Height), int(p.FrameCount()), a.frameBuf.maxBytes)
	a.Window.Option(app.Title(fmt.Sprintf("%s — Playback (%d frames)", a.modelName, p.FrameCount())))
}

// updateBuffer adds the colorized celsius frame to the appropriate ring or
// playback buffer, resizing if the image dimensions changed.
func (a *App) updateBuffer(result *colorize.Result) {
	imgW := result.RGBA.Bounds().Dx()
	imgH := result.RGBA.Bounds().Dy()
	if a.playBuf != nil {
		frameIdx := a.player.FrameIndex() - 1
		if frameIdx < 0 {
			frameIdx = 0
		}
		if bw, bh := a.playBuf.Dims(); bw != imgW || bh != imgH {
			a.playBuf.Resize(imgW, imgH)
		}
		a.playBuf.Add(frameIdx, result.Celsius, a.player.FrameTime())
	} else {
		if bw, bh := a.frameBuf.Dims(); bw != imgW || bh != imgH {
			a.frameBuf.Resize(imgW, imgH)
		}
		a.frameBuf.Add(result.Celsius, time.Now())
	}
}

// updateEMASpot updates spot's position using EMA (or initializes it on first frame)
// and records the temperature at its position.
func (a *App) updateEMASpot(spot *Spot, newX, newY float32, alpha float32, celsius []float32, imgW int) {
	if !a.smInited.Load() {
		spot.SetPosition(newX, newY, true)
	} else {
		spot.UpdateEMA(newX, newY, alpha)
	}
	sx, sy := spot.GetPosition()
	if idx := int(sy)*imgW + int(sx); idx >= 0 && idx < len(celsius) {
		spot.Record(celsius[idx])
	}
}

func (a *App) UpdateFrame(frame *camera.Frame) {
	a.mu.Lock()
	params := a.params
	rot := a.rotation
	a.lastFrame = frame
	a.shutterActive = frame.ShutterActive
	rec := a.recorder
	a.mu.Unlock()

	// Record frame if recording is active
	if rec != nil {
		if err := rec.WriteFrame(frame); err != nil {
			log.Printf("recording write: %v", err)
		}
	}

	result := colorize.Colorize(frame, params).Rotate(rot)

	a.mu.Lock()
	a.result = result
	a.mu.Unlock()

	imgW := result.RGBA.Bounds().Dx()
	a.updateBuffer(result)

	// Update EMA-smoothed min/max spots
	const alpha = 0.15
	a.updateEMASpot(a.spots[0], float32(result.MinX), float32(result.MinY), alpha, result.Celsius, imgW)
	a.updateEMASpot(a.spots[1], float32(result.MaxX), float32(result.MaxY), alpha, result.Celsius, imgW)
	a.smInited.Store(true)

	a.updateCursorSpot(result, imgW)
	a.updateUserSpots(result, imgW)
	a.invalidateGraphs()
}

// updateCursorSpot refreshes the cursor spot temperature from the current result.
func (a *App) updateCursorSpot(result *colorize.Result, imgW int) {
	cursorSpot := a.spots[spotCursorIdx]
	cursorState := cursorSpot.GetState()

	if !cursorState.Active {
		return
	}

	if cIdx := int(cursorState.Y)*imgW + int(cursorState.X); cIdx >= 0 && cIdx < len(result.Celsius) {
		cursorSpot.SetLastTemp(result.Celsius[cIdx])
	}
}

// updateUserSpots refreshes temperature readings for all user-placed spots.
func (a *App) updateUserSpots(result *colorize.Result, imgW int) {
	a.mu.Lock()
	userSpots := make([]*Spot, len(a.spots[firstUserSpotIdx:]))
	copy(userSpots, a.spots[firstUserSpotIdx:])
	a.mu.Unlock()

	for _, sp := range userSpots {
		st := sp.GetState()
		if idx := int(st.Y)*imgW + int(st.X); idx >= 0 && idx < len(result.Celsius) {
			temp := sp.CorrectedTemp(result.Celsius[idx], result.GlobalEmissivity, result.AmbientC)
			sp.SetLastTemp(temp)
		}
	}
}

// refreshDisplay re-colorizes the last raw frame with the current params.
// Call after changing palette, emissivity, AGC mode, or rotation while paused.
func (a *App) refreshDisplay() {
	a.mu.Lock()
	frame := a.lastFrame
	params := a.params
	rot := a.rotation
	a.mu.Unlock()

	if frame == nil {
		return
	}

	result := colorize.Colorize(frame, params).Rotate(rot)

	a.mu.Lock()
	a.result = result
	a.mu.Unlock()

	imgW := result.RGBA.Bounds().Dx()

	// Re-read spot temperatures at their current positions.
	for spotIdx, spot := range a.spots {
		st := spot.GetState()
		if !st.Active {
			continue
		}
		idx := int(st.Y)*imgW + int(st.X)
		if idx < 0 || idx >= len(result.Celsius) {
			continue
		}
		if spotIdx >= firstUserSpotIdx {
			temp := spot.CorrectedTemp(result.Celsius[idx], result.GlobalEmissivity, result.AmbientC)
			spot.SetLastTemp(temp)
		} else {
			spot.SetLastTemp(result.Celsius[idx])
		}
	}

	a.Window.Invalidate()
}

func (a *App) Run() error {
	var ops op.Ops
	for {
		switch winEv := a.Window.Event().(type) {
		case app.DestroyEvent:
			return winEv.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, winEv)
			// Fill window background black
			paint.Fill(gtx.Ops, color.NRGBA{A: 255})
			a.handleKeys(gtx)
			a.doLayout(gtx)
			winEv.Frame(gtx.Ops)
		}
	}
}

// handleEscKey handles Escape: close dropdown/panel/deselect/quit.
func (a *App) handleEscKey(keyName key.Name) {
	switch {
	case keyName == key.NameEscape && a.epsDropdown.IsOpen():
		a.epsDropdown.Close()
	case keyName == key.NameEscape && a.bufPanel.IsOpen():
		a.bufPanel.Close()
	case keyName == key.NameEscape && a.selectedSpot >= 0:
		a.selectedSpot = -1
	default:
		a.Window.Perform(system.ActionClose)
	}
}

// handleEKey cycles emissivity for the selected spot (or globally).
func (a *App) handleEKey(backward bool) {
	nPresets := len(colorize.EmissivityPresets)
	a.mu.Lock()

	if a.selectedSpot >= 0 && a.selectedSpot < len(a.spots) {
		a.cycleSpotEmissivity(a.spots[a.selectedSpot], backward, nPresets)
	} else {
		a.cycleGlobalEmissivity(backward, nPresets)
	}

	a.toastExpiry = time.Now().Add(toastShortDuration)
	a.mu.Unlock()
	a.refreshDisplay()
}

// cycleSpotEmissivity advances or reverses the emissivity preset for a single spot.
// Caller must hold a.mu.
func (a *App) cycleSpotEmissivity(selSpot *Spot, backward bool, nPresets int) {
	_, curIdx := selSpot.GetEmissivity()
	if backward {
		curIdx--
		if curIdx < -1 {
			curIdx = nPresets - 1
		}
	} else {
		curIdx++
		if curIdx >= nPresets {
			curIdx = -1
		}
	}

	if curIdx == -1 {
		selSpot.SetEmissivity(0, -1)
		a.toastMsg = fmt.Sprintf("Spot %d: ε = global", a.selectedSpot)
	} else {
		preset := colorize.EmissivityPresets[curIdx]
		selSpot.SetEmissivity(preset.Emissivity, curIdx)
		a.toastMsg = fmt.Sprintf("Spot %d: ε = %.2f  %s", a.selectedSpot, preset.Emissivity, preset.Name)
	}
}

// cycleGlobalEmissivity advances or reverses the global emissivity preset.
// Caller must hold a.mu.
func (a *App) cycleGlobalEmissivity(backward bool, nPresets int) {
	if backward {
		a.emissivityIdx = (a.emissivityIdx - 1 + nPresets) % nPresets
	} else {
		a.emissivityIdx = (a.emissivityIdx + 1) % nPresets
	}

	preset := colorize.EmissivityPresets[a.emissivityIdx]
	a.params.Emissivity = preset.Emissivity
	a.toastMsg = fmt.Sprintf("ε = %.2f  %s", preset.Emissivity, preset.Name)
}

// handlePlayPause toggles playback and shows a toast.
func (a *App) handlePlayPause() {
	if a.player == nil {
		return
	}
	paused := a.player.IsPaused()
	a.player.SetPaused(!paused)
	a.mu.Lock()
	if !paused {
		a.toastMsg = "Playback paused"
	} else {
		a.toastMsg = "Playback resumed"
	}
	a.toastExpiry = time.Now().Add(toastShortDuration)
	a.mu.Unlock()
}

// dumpLastFrame dumps the last raw frame to disk in a goroutine.
func (a *App) dumpLastFrame() {
	a.mu.Lock()
	frame := a.lastFrame
	a.mu.Unlock()

	if frame != nil {
		go a.dumpFrame(frame)
	}
}

// dumpScreenshot saves the current RGBA frame as a PNG in a goroutine.
func (a *App) dumpScreenshot() {
	a.mu.Lock()
	snap := a.result
	a.mu.Unlock()

	if snap != nil && snap.RGBA != nil {
		go a.saveScreenshot(snap.RGBA)
	}
}

// seekBackward steps the player one frame backward.
func (a *App) seekBackward() {
	if a.player == nil {
		return
	}

	idx := a.player.FrameIndex() - seekBackStep
	if idx < 0 {
		idx = 0
	}

	a.seekToFrame(idx)
}

// seekForward steps the player one frame forward.
func (a *App) seekForward() {
	if a.player != nil {
		a.seekToFrame(a.player.FrameIndex())
	}
}

// toggleGain switches the camera between high and low gain modes.
func (a *App) toggleGain() {
	if a.gainMode == camera.GainHigh {
		a.gainMode = camera.GainLow
	} else {
		a.gainMode = camera.GainHigh
	}
	newGain := a.gainMode
	go func() {
		log.Printf("switching gain to %v", newGain)
		if err := a.cam.SetGain(newGain); err != nil {
			log.Printf("gain switch: %v", err)
		}
	}()
}

// closeUserSpotGraphs closes all graph windows for user spots (index >= 3)
// and removes those spots. Caller must NOT hold a.mu.
func (a *App) closeUserSpotGraphs() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for idx, gw := range a.graphs {
		if idx >= firstUserSpotIdx {
			gw.mu.Lock()
			gw.window.Perform(system.ActionClose)
			gw.mu.Unlock()
			delete(a.graphs, idx)
		}
	}

	a.spots = a.spots[:firstUserSpotIdx]
	a.selectedSpot = -1
}

// cycleAGC toggles between Hardware and Percentile AGC modes.
func (a *App) cycleAGC() {
	a.mu.Lock()
	if a.params.Mode == colorize.AGCHardware {
		a.params.Mode = colorize.AGCPercentile
	} else {
		a.params.Mode = colorize.AGCHardware
	}
	a.mu.Unlock()
	a.refreshDisplay()
}

func (a *App) handleKeys(gtx layout.Context) {
	// Register key filters
	filters := []event.Filter{
		key.Filter{Name: "Q"},
		key.Filter{Name: key.NameEscape},
		key.Filter{Name: "C"},
		key.Filter{Name: "A"},
		key.Filter{Name: "G"},
		key.Filter{Name: "S"},
		key.Filter{Name: "H"},
		key.Filter{Name: "V"},
		key.Filter{Name: key.NameSpace},
		key.Filter{Name: key.NameF12},
		key.Filter{Name: "R"},
		key.Filter{Name: "T"},
		key.Filter{Name: "E", Optional: key.ModShift},
		key.Filter{Name: "X"},
		key.Filter{Name: "D"},
		key.Filter{Name: key.NameF5},
		key.Filter{Name: "P"},
		key.Filter{Name: key.NameLeftArrow},
		key.Filter{Name: key.NameRightArrow},
		// Number keys for graph toggle
		key.Filter{Name: "0"},
		key.Filter{Name: "1"},
		key.Filter{Name: "2"},
		key.Filter{Name: "3"},
		key.Filter{Name: "4"},
		key.Filter{Name: "5"},
		key.Filter{Name: "6"},
		key.Filter{Name: "7"},
		key.Filter{Name: "8"},
		key.Filter{Name: "9"},
	}

	for {
		inputEv, ok := gtx.Source.Event(filters...)
		if !ok {
			break
		}
		keyEv, ok := inputEv.(key.Event)
		if !ok || keyEv.State != key.Press {
			continue
		}

		a.dispatchKeyEvent(keyEv)
	}
}

// dispatchKeyEvent routes a key press to the appropriate handler.
// The high cyclomatic complexity here is inherent: the app has many key bindings.
//
//nolint:cyclop
func (a *App) dispatchKeyEvent(keyEv key.Event) {
	switch keyEv.Name {
	case "Q", key.NameEscape:
		a.handleEscKey(keyEv.Name)
	case "C":
		a.mu.Lock()
		a.params.Palette = a.params.Palette.Next()
		a.mu.Unlock()
		a.refreshDisplay()
	case "A":
		a.cycleAGC()
	case "G":
		a.toggleGain()
	case "S":
		a.triggerShutter()
	case "V":
		a.showColorbar = !a.showColorbar
	case "H":
		a.showHelp = !a.showHelp
	case "R":
		a.rotation = (a.rotation + 1) % rotationCount
		a.refreshDisplay()
	case "T":
		a.showLabels = !a.showLabels
	case "E":
		a.handleEKey(keyEv.Modifiers.Contain(key.ModShift))
	case "X":
		a.closeUserSpotGraphs()
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		a.toggleGraph(int(keyEv.Name[0] - '0'))
	case "D":
		a.dumpLastFrame()
	case key.NameF5:
		a.toggleRecording()
	case "P", key.NameSpace:
		a.handlePlayPause()
	case key.NameLeftArrow:
		a.seekBackward()
	case key.NameRightArrow:
		a.seekForward()
	case key.NameF12:
		a.dumpScreenshot()
	}
}

// triggerShutter fires the camera shutter in a background goroutine.
func (a *App) triggerShutter() {
	go func() {
		if err := a.cam.TriggerShutter(); err != nil {
			log.Printf("shutter: %v", err)
		}
	}()
}

// handlePointerMove updates the cursor spot position and temperature when the
// pointer moves over the image area.
func (a *App) handlePointerMove(ptrEv pointer.Event) {
	a.cursorPos = ptrEv.Position
	imgX := int((ptrEv.Position.X - float32(a.imgOffsetX)) / a.imgScale)
	imgY := int((ptrEv.Position.Y - float32(a.imgOffsetY)) / a.imgScale)

	a.mu.Lock()
	res := a.result
	a.mu.Unlock()

	cursorSpot := a.spots[spotCursorIdx]
	bounds := res != nil && imgX >= 0 && imgY >= 0 && imgX < res.RGBA.Bounds().Dx() && imgY < res.RGBA.Bounds().Dy()
	if bounds {
		cursorSpot.SetPosition(float32(imgX), float32(imgY), true)
		imgW := res.RGBA.Bounds().Dx()
		if cIdx := imgY*imgW + imgX; cIdx < len(res.Celsius) {
			cursorSpot.SetLastTemp(res.Celsius[cIdx])
		}
	} else {
		cursorSpot.SetActive(false)
	}

	a.mu.Lock()
	cursorGW := a.graphs[spotCursorIdx]
	a.mu.Unlock()
	if cursorGW != nil && !cursorGW.IsClosed() {
		cursorGW.Invalidate()
	}
}

// handlePointerShiftPress handles Shift+Click: select/deselect a user spot for
// per-spot emissivity assignment.
func (a *App) handlePointerShiftPress(imgX, imgY int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	found := -1
	for spotIdx := firstUserSpotIdx; spotIdx < len(a.spots); spotIdx++ {
		spX, spY := a.spots[spotIdx].GetPosition()
		dx := int(spX) - imgX
		dy := int(spY) - imgY
		if dx*dx+dy*dy < spotHitRadiusSq {
			found = spotIdx

			break
		}
	}
	if found < 0 {
		return
	}

	if a.selectedSpot == found {
		a.selectedSpot = -1
	} else {
		a.selectedSpot = found
	}
}

// removeUserSpot removes the user spot at spotIdx, closes its graph window,
// and renumbers remaining spots. Caller must hold a.mu.
func (a *App) removeUserSpot(spotIdx int) {
	if a.selectedSpot == spotIdx {
		a.selectedSpot = -1
	} else if a.selectedSpot > spotIdx {
		a.selectedSpot--
	}

	if gw, ok := a.graphs[spotIdx]; ok {
		gw.mu.Lock()
		gw.window.Perform(system.ActionClose)
		gw.mu.Unlock()
		delete(a.graphs, spotIdx)
	}

	a.spots = append(a.spots[:spotIdx], a.spots[spotIdx+1:]...)

	newGraphs := make(map[int]*GraphWindow)
	for k, v := range a.graphs {
		if k < firstUserSpotIdx {
			newGraphs[k] = v
		}
	}
	for j := firstUserSpotIdx; j < len(a.spots); j++ {
		a.spots[j].Index = j
		if gw, ok := a.graphs[j+1]; ok {
			newGraphs[j] = gw
		}
	}
	a.graphs = newGraphs
}

// handlePointerNormalPress handles a plain click: add or remove a user spot at
// the given image coordinates.
func (a *App) handlePointerNormalPress(imgX, imgY int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for spotIdx := firstUserSpotIdx; spotIdx < len(a.spots); spotIdx++ {
		spX, spY := a.spots[spotIdx].GetPosition()
		dx := int(spX) - imgX
		dy := int(spY) - imgY
		if dx*dx+dy*dy < spotHitRadiusSq {
			a.removeUserSpot(spotIdx)

			return
		}
	}

	newIdx := len(a.spots)
	newSpot := NewSpot(newIdx, SpotUser, color.NRGBA{R: 60, G: 220, B: 60, A: 230})
	newSpot.SetPosition(float32(imgX), float32(imgY), true)
	a.spots = append(a.spots, newSpot)
}

func (a *App) handlePointer(gtx layout.Context) {
	// Register for pointer move events in the image area
	filters := []event.Filter{
		pointer.Filter{
			Target: &a.tag,
			Kinds:  pointer.Move | pointer.Enter | pointer.Leave | pointer.Press,
		},
	}

	for {
		inputEv, ok := gtx.Source.Event(filters...)
		if !ok {
			break
		}
		ptrEv, ok := inputEv.(pointer.Event)
		if !ok {
			continue
		}

		switch ptrEv.Kind {
		case pointer.Move, pointer.Enter:
			a.handlePointerMove(ptrEv)

		case pointer.Press:
			a.handlePointerPress(ptrEv)

		case pointer.Leave:
			a.spots[spotCursorIdx].SetActive(false)
		}
	}
}

// handlePointerPress handles a pointer press event: bounds-checks the click
// position and dispatches to shift-press or normal-press handling.
func (a *App) handlePointerPress(ptrEv pointer.Event) {
	imgX := int((ptrEv.Position.X - float32(a.imgOffsetX)) / a.imgScale)
	imgY := int((ptrEv.Position.Y - float32(a.imgOffsetY)) / a.imgScale)

	a.mu.Lock()
	res := a.result
	a.mu.Unlock()

	inBounds := res != nil && imgX >= 0 && imgY >= 0 &&
		imgX < res.RGBA.Bounds().Dx() && imgY < res.RGBA.Bounds().Dy()
	if !inBounds {
		return
	}

	if ptrEv.Modifiers.Contain(key.ModShift) {
		a.handlePointerShiftPress(imgX, imgY)
	} else {
		a.handlePointerNormalPress(imgX, imgY)
	}
}

// applyBufPanelSizeChange handles a size change from the buffer panel.
func (a *App) applyBufPanelSizeChange(newBytes int64) {
	a.frameBuf.SetMaxBytes(newBytes)
	if a.playBuf != nil {
		a.playBuf.StopBackfill()
		a.playBuf.SetMaxBytes(newBytes)
		idx := a.player.FrameIndex()
		a.mu.Lock()
		params := a.params
		rot := a.rotation
		a.mu.Unlock()
		a.playBuf.StartBackfill(a.player, idx, params, rot, a.invalidateGraphs)
	}
	a.mu.Lock()
	a.toastMsg = fmt.Sprintf("Buffer resized to %s", humanSize(newBytes))
	a.toastExpiry = time.Now().Add(toastShortDuration)
	a.mu.Unlock()
}

// applyBufPanelIntervalChange handles a sample-interval change from the buffer panel.
func (a *App) applyBufPanelIntervalChange(newInterval time.Duration) {
	a.frameBuf.SetSampleInterval(newInterval)
	if a.playBuf != nil {
		skip := 1
		if newInterval > 0 {
			skip = int(newInterval.Milliseconds() / msPerFrameAt25FPS)
			if skip < 1 {
				skip = 1
			}
		}
		a.playBuf.StopBackfill()
		a.playBuf.SetSampleSkip(skip)
		a.playBuf.Clear()
		idx := a.player.FrameIndex()
		a.mu.Lock()
		params := a.params
		rot := a.rotation
		a.mu.Unlock()
		a.playBuf.StartBackfill(a.player, idx, params, rot, a.invalidateGraphs)
	}
	label := "Max"
	for _, p := range sampleRatePresets {
		if p.Interval == newInterval {
			label = p.Label

			break
		}
	}
	a.mu.Lock()
	a.toastMsg = fmt.Sprintf("Sample rate: %s", label)
	a.toastExpiry = time.Now().Add(toastShortDuration)
	a.mu.Unlock()
}

func (a *App) doLayout(gtx layout.Context) layout.Dimensions {
	a.mu.Lock()
	result := a.result
	a.mu.Unlock()

	if result == nil || result.RGBA == nil {
		return layout.Dimensions{Size: gtx.Constraints.Max}
	}

	children := []layout.FlexChild{
		// Thermal image
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return a.layoutImage(gtx, result)
		}),
	}

	// Colorbar
	if a.showColorbar {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return a.layoutColorbar(gtx, result)
		}))
	}

	// Playback bar (only in playback mode)
	if a.player != nil {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return a.layoutPlaybackBar(gtx)
		}))
	}

	// Status bar
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return a.layoutStatus(gtx, result)
	}))

	// Help overlay is drawn on top after the main layout
	dims := layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)

	a.layoutOverlays(gtx)

	return dims
}

// layoutOverlays renders the help panel, emissivity dropdown, buffer panel,
// and toast notification on top of the main layout.
func (a *App) layoutOverlays(gtx layout.Context) {
	if a.showHelp {
		a.layoutHelp(gtx)
	}

	a.layoutEpsDropdownOverlay(gtx)
	a.layoutBufPanelOverlay(gtx)
	a.layoutToastOverlay(gtx)
}

// layoutEpsDropdownOverlay renders the emissivity dropdown if it is open.
func (a *App) layoutEpsDropdownOverlay(gtx layout.Context) {
	if !a.epsDropdown.IsOpen() {
		return
	}

	a.mu.Lock()
	eIdx := a.emissivityIdx
	a.mu.Unlock()

	if sel := a.epsDropdown.Layout(gtx, a.theme, eIdx); sel >= 0 {
		a.mu.Lock()
		a.emissivityIdx = sel
		preset := colorize.EmissivityPresets[sel]
		a.params.Emissivity = preset.Emissivity
		a.toastMsg = fmt.Sprintf("ε = %.2f  %s", preset.Emissivity, preset.Name)
		a.toastExpiry = time.Now().Add(toastShortDuration)
		a.mu.Unlock()
		a.refreshDisplay()
	}
}

// layoutBufPanelOverlay renders the buffer-settings panel if it is open.
func (a *App) layoutBufPanelOverlay(gtx layout.Context) {
	if !a.bufPanel.IsOpen() {
		return
	}

	curBytes := a.frameBuf.MaxBytes()
	curInterval := a.frameBuf.SampleInterval()
	w, h := a.frameBuf.Dims()
	res := a.bufPanel.Layout(gtx, a.theme, curBytes, curInterval, frameBytesPerPixel, w*h)

	if res.SizeChanged {
		a.applyBufPanelSizeChange(res.NewBytes)
	}

	if res.IntervalChanged {
		a.applyBufPanelIntervalChange(res.NewInterval)
	}
}

// layoutToastOverlay renders the toast notification if it is active.
func (a *App) layoutToastOverlay(gtx layout.Context) {
	a.mu.Lock()
	msg := a.toastMsg
	expiry := a.toastExpiry
	a.mu.Unlock()

	if msg != "" && time.Now().Before(expiry) {
		a.layoutToast(gtx, msg)
		a.Window.Invalidate()
	}
}

// drawSpotMarkers paints colored squares (and optional selection rings) for all
// non-cursor spots onto the image area.
func (a *App) drawSpotMarkers(
	gtx layout.Context, snaps []spotSnap, selIdx, offsetX, offsetY, markerSize int, scale float32,
) {
	for _, snap := range snaps {
		if !snap.state.Active || snap.sp.Kind == SpotCursor {
			continue
		}

		markerCX := offsetX + int(snap.state.X*scale+scale/2)
		markerCY := offsetY + int(snap.state.Y*scale+scale/2)

		// Selection highlight: larger yellow ring
		if snap.sp.Index == selIdx {
			ringSize := markerSize + selectionRingExtra
			selRingRect := image.Rectangle{
				Min: image.Pt(markerCX-ringSize, markerCY-ringSize),
				Max: image.Pt(markerCX+ringSize, markerCY+ringSize),
			}
			selRingOp := clip.Rect(selRingRect).Push(gtx.Ops)
			paint.Fill(gtx.Ops, color.NRGBA{R: 255, G: 220, B: 0, A: 255})
			selRingOp.Pop()
		}

		mx := markerCX - markerSize
		my := markerCY - markerSize
		sz := markerSize * markerDiameterMult
		markerOp := clip.Rect{Min: image.Pt(mx, my), Max: image.Pt(mx+sz, my+sz)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, snap.sp.Color)
		markerOp.Pop()
	}
}

// drawSpotLabels paints temperature labels for all visible non-cursor spots.
func (a *App) drawSpotLabels(
	gtx layout.Context, snaps []spotSnap, result *colorize.Result, offsetX, offsetY int, scale float32,
) {
	imgW := result.RGBA.Bounds().Dx()
	for _, snap := range snaps {
		if !snap.state.Active || snap.sp.Kind == SpotCursor {
			continue
		}
		idx := int(snap.state.Y)*imgW + int(snap.state.X)
		if idx < 0 || idx >= len(result.Celsius) {
			continue
		}
		labelX := offsetX + int(snap.state.X*scale+scale/2)
		labelY := offsetY + int(snap.state.Y*scale) - spotLabelYAdj
		temp := snap.sp.CorrectedTemp(result.Celsius[idx], result.GlobalEmissivity, result.AmbientC)
		epsSuffix := ""
		if snap.state.Emissivity > 0 {
			epsSuffix = fmt.Sprintf(" e%.2f", snap.state.Emissivity)
		}
		a.drawSpotLabel(gtx, labelX, labelY, snap.sp.Index, temp, epsSuffix, snap.sp.Color)
	}
}

func (a *App) layoutImage(gtx layout.Context, result *colorize.Result) layout.Dimensions {
	img := result.RGBA
	if img == nil {
		return layout.Dimensions{Size: gtx.Constraints.Max}
	}

	bounds := img.Bounds()
	imgW := float32(bounds.Dx())
	imgH := float32(bounds.Dy())

	availW := float32(gtx.Constraints.Max.X)
	availH := float32(gtx.Constraints.Max.Y)

	scaleX := availW / imgW
	scaleY := availH / imgH
	scale := scaleX
	if scaleY < scale {
		scale = scaleY
	}

	scaledW := int(math.Floor(float64(imgW * scale)))
	scaledH := int(math.Floor(float64(imgH * scale)))

	offsetX := (int(availW) - scaledW) / centerDiv
	offsetY := (int(availH) - scaledH) / centerDiv

	// Store layout info for cursor mapping
	a.imgOffsetX = offsetX
	a.imgOffsetY = offsetY
	a.imgScale = scale

	// Register pointer input area
	area := clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops)
	event.Op(gtx.Ops, &a.tag)
	area.Pop()
	a.handlePointer(gtx)

	// Draw the scaled thermal image
	{
		offsetOp := op.Offset(image.Pt(offsetX, offsetY)).Push(gtx.Ops)
		clipOp := clip.Rect{Max: image.Pt(scaledW, scaledH)}.Push(gtx.Ops)
		aff := f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(scale, scale))
		scaleOp := op.Affine(aff).Push(gtx.Ops)

		imgOp := paint.NewImageOp(img)
		imgOp.Filter = paint.FilterNearest
		imgOp.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)

		scaleOp.Pop()
		clipOp.Pop()
		offsetOp.Pop()
	}

	// Draw spot markers and labels
	markerSize := 4
	a.mu.Lock()
	allSpots := make([]*Spot, len(a.spots))
	copy(allSpots, a.spots)
	selIdx := a.selectedSpot
	a.mu.Unlock()

	// Take a snapshot of each spot's mutable state (thread-safe)
	snaps := make([]spotSnap, len(allSpots))
	for snapIdx, sp := range allSpots {
		snaps[snapIdx] = spotSnap{sp: sp, state: sp.GetState()}
	}

	a.drawSpotMarkers(gtx, snaps, selIdx, offsetX, offsetY, markerSize, scale)

	// Temperature labels
	if a.showLabels {
		a.drawSpotLabels(gtx, snaps, result, offsetX, offsetY, scale)
	}

	// Cursor temperature label (next to mouse pointer)
	cursorSpot := a.spots[spotCursorIdx]
	cursorState := snaps[spotCursorIdx].state
	if cursorState.Active {
		cursorLX := int(a.cursorPos.X) + cursorLabelXOff
		cursorLY := int(a.cursorPos.Y) - cursorLabelYOff
		a.drawTempLabel(gtx, cursorLX, cursorLY, cursorSpot.LastTemp(), cursorSpot.Color)
	}

	// NUC (shutter) indicator — top-right of image area
	a.mu.Lock()
	nucActive := a.shutterActive
	a.mu.Unlock()
	if nucActive {
		a.drawNUCIndicator(gtx, offsetX+scaledW, offsetY)
	}

	return layout.Dimensions{Size: gtx.Constraints.Max}
}

// drawSpotLabel draws a temperature label with the spot index prefix.
func (a *App) drawSpotLabel(
	gtx layout.Context, labelX, labelY int, index int, temp float32, suffix string, col color.NRGBA,
) {
	txt := fmt.Sprintf("[%d] %.1f\u00b0%s", index, temp, suffix)

	// Measure
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	dims := layout.Inset{
		Left: unit.Dp(labelInsetMdDp), Right: unit.Dp(labelInsetMdDp),
		Top: unit.Dp(labelInsetSmDp), Bottom: unit.Dp(labelInsetSmDp),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(a.theme, txt)
		lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}

		return lbl.Layout(gtx)
	})
	call := macro.Stop()

	ox := labelX - dims.Size.X/labelHCenterDiv
	oy := labelY - dims.Size.Y
	offsetOp := op.Offset(image.Pt(ox, oy)).Push(gtx.Ops)

	pill := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
	paint.Fill(gtx.Ops, col)
	pill.Pop()

	call.Add(gtx.Ops)
	offsetOp.Pop()
}

// toggleGraph opens or closes a graph window for the spot at the given index.
func (a *App) toggleGraph(idx int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if idx >= len(a.spots) {
		return
	}

	if gw, ok := a.graphs[idx]; ok && !gw.IsClosed() {
		// Close it by performing close action
		gw.mu.Lock()
		gw.window.Perform(system.ActionClose)
		gw.mu.Unlock()
		delete(a.graphs, idx)

		return
	}

	// Open new graph window with the appropriate data source
	var pixSrc PixelQuerier = a.frameBuf
	if a.playBuf != nil {
		pixSrc = a.playBuf
	}
	gw := NewGraphWindow(a.spots[idx], pixSrc)
	a.graphs[idx] = gw
}

// drawTempLabel draws a temperature reading with a small background tag at the given screen position.
func (a *App) drawTempLabel(gtx layout.Context, labelX, labelY int, temp float32, col color.NRGBA) {
	txt := fmt.Sprintf("%.1f\u00b0", temp)

	// Measure
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	dims := layout.Inset{
		Left: unit.Dp(labelInsetMdDp), Right: unit.Dp(labelInsetMdDp),
		Top: unit.Dp(labelInsetSmDp), Bottom: unit.Dp(labelInsetSmDp),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(a.theme, txt)
		lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}

		return lbl.Layout(gtx)
	})
	call := macro.Stop()

	// Position: center horizontally above the marker
	ox := labelX - dims.Size.X/labelHCenterDiv
	oy := labelY - dims.Size.Y
	offsetOp := op.Offset(image.Pt(ox, oy)).Push(gtx.Ops)

	// Background pill
	pill := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
	paint.Fill(gtx.Ops, col)
	pill.Pop()

	call.Add(gtx.Ops)
	offsetOp.Pop()
}

// drawNUCIndicator draws a "NUC" badge at the top-right of the image area.
func (a *App) drawNUCIndicator(gtx layout.Context, rightX, topY int) {
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	dims := layout.Inset{
		Left: unit.Dp(labelInsetLgDp), Right: unit.Dp(labelInsetLgDp),
		Top: unit.Dp(labelInsetMdDp), Bottom: unit.Dp(labelInsetMdDp),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Body1(a.theme, "NUC")
		lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
		lbl.Font.Weight = font.Bold

		return lbl.Layout(gtx)
	})
	call := macro.Stop()

	ox := rightX - dims.Size.X - gtx.Dp(unit.Dp(nucBadgeInsetDp))
	oy := topY + gtx.Dp(unit.Dp(nucBadgeInsetDp))
	offsetOp := op.Offset(image.Pt(ox, oy)).Push(gtx.Ops)

	bg := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
	paint.Fill(gtx.Ops, color.NRGBA{R: 200, G: 40, B: 40, A: 220})
	bg.Pop()

	call.Add(gtx.Ops)
	offsetOp.Pop()
}

func (a *App) layoutColorbar(gtx layout.Context, result *colorize.Result) layout.Dimensions {
	barH := gtx.Dp(unit.Dp(colorbarHeightDp))
	barImg := colorize.MakeColorbar(a.params.Palette, barH)

	// Scale colorbar to full width
	barW := gtx.Constraints.Max.X
	scaleX := float32(barW) / float32(barImg.Bounds().Dx())
	scaleY := float32(barH) / float32(barImg.Bounds().Dy())

	// Draw the gradient bar
	{
		barClipOp := clip.Rect{Max: image.Pt(barW, barH)}.Push(gtx.Ops)
		aff := f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(scaleX, scaleY))
		barScaleOp := op.Affine(aff).Push(gtx.Ops)

		imgOp := paint.NewImageOp(barImg)
		imgOp.Filter = paint.FilterLinear
		imgOp.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)

		barScaleOp.Pop()
		barClipOp.Pop()
	}

	// Pick contrasting text colors based on LUT endpoints
	lut := a.params.Palette.LUT()
	minBg := lut[0]
	maxBg := lut[255]
	contrastColor := func(bg [3]uint8) color.NRGBA {
		// Perceived luminance
		lum := lumWeightR*float32(bg[0]) + lumWeightG*float32(bg[1]) + lumWeightB*float32(bg[2])
		if lum > lumThreshold {
			return color.NRGBA{A: 255} // black
		}

		return color.NRGBA{R: 255, G: 255, B: 255, A: 255} // white
	}

	// Min label (left)
	{
		s := op.Offset(image.Pt(colorbarLabelXOffset, 1)).Push(gtx.Ops)
		lbl := material.Caption(a.theme, fmt.Sprintf("%.1f\u00b0C", result.MinC))
		lbl.Color = contrastColor(minBg)
		lbl.Layout(gtx)
		s.Pop()
	}

	// Max label (right)
	{
		maxStr := fmt.Sprintf("%.1f\u00b0C", result.MaxC)
		s := op.Offset(image.Pt(barW-gtx.Dp(unit.Dp(colorbarMaxLabelInsetDp)), 1)).Push(gtx.Ops)
		lbl := material.Caption(a.theme, maxStr)
		lbl.Color = contrastColor(maxBg)
		lbl.Layout(gtx)
		s.Pop()
	}

	return layout.Dimensions{Size: image.Pt(barW, barH)}
}

func (a *App) layoutPlaybackBar(gtx layout.Context) layout.Dimensions {
	player := a.player
	if player == nil {
		return layout.Dimensions{}
	}

	total := int(player.FrameCount())
	if total == 0 {
		total = 1
	}
	current := player.FrameIndex()
	if current > total {
		current = total
	}
	paused := player.IsPaused()

	barH := gtx.Dp(unit.Dp(playbackBarHeightDp))
	lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}
	bgColor := color.NRGBA{R: 35, G: 35, B: 35, A: 255}

	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
			paint.Fill(gtx.Ops, bgColor)

			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(playbarInsetDp)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					// Play/Pause button
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if a.playPauseClick.Clicked(gtx) {
							player.SetPaused(!paused)
						}

						return material.Clickable(gtx, &a.playPauseClick, func(gtx layout.Context) layout.Dimensions {
							btnBg := color.NRGBA{R: 50, G: 50, B: 50, A: 255}
							if a.playPauseClick.Hovered() {
								btnBg = color.NRGBA{R: 70, G: 70, B: 70, A: 255}
							}

							return layout.Background{}.Layout(gtx,
								func(gtx layout.Context) layout.Dimensions {
									defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
									paint.Fill(gtx.Ops, btnBg)

									return layout.Dimensions{Size: gtx.Constraints.Min}
								},
								func(gtx layout.Context) layout.Dimensions {
									return layout.Inset{
										Left: unit.Dp(playBtnInsetXDp), Right: unit.Dp(playBtnInsetXDp),
										Top: unit.Dp(playBtnInsetYDp), Bottom: unit.Dp(playBtnInsetYDp),
									}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										txt := "PLAY"
										if !paused {
											txt = "PAUS"
										}
										lbl := material.Body2(a.theme, txt)
										lbl.Color = lightGray
										lbl.Font.Weight = font.Bold

										return lbl.Layout(gtx)
									})
								},
							)
						})
					}),
					// Slider (Flexed — left edge anchored next to button)
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return a.layoutSlider(gtx, current, total, barH)
					}),
					// Frame counter + absolute time (right side, Rigid)
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						inset := layout.Inset{
							Left:  unit.Dp(frameCounterInsetDp),
							Right: unit.Dp(frameCounterRDp),
						}

						return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							ft := player.FrameTime()
							txt := fmt.Sprintf("%d/%d  %s", current, total, ft.Format("2006-01-02 15:04:05"))
							lbl := material.Body2(a.theme, txt)
							lbl.Color = lightGray

							return lbl.Layout(gtx)
						})
					}),
				)
			})
		},
	)
}

// handleSliderEvents processes pointer input on the playback slider area.
func (a *App) handleSliderEvents(gtx layout.Context, sliderW, total, current int) {
	filters := []event.Filter{
		pointer.Filter{
			Target:  &a.sliderTag,
			Kinds:   pointer.Press | pointer.Drag | pointer.Release | pointer.Scroll,
			ScrollY: pointer.ScrollRange{Min: -10, Max: 10},
		},
	}

	for {
		inputEv, ok := gtx.Source.Event(filters...)
		if !ok {
			break
		}

		ptrEv, isPtr := inputEv.(pointer.Event)
		if !isPtr {
			continue
		}

		switch ptrEv.Kind {
		case pointer.Press:
			a.sliderDragging = true
			a.seekToFrame(a.sliderPosToFrame(ptrEv.Position.X, float32(sliderW), total))

		case pointer.Drag:
			if a.sliderDragging {
				a.seekToFrame(a.sliderPosToFrame(ptrEv.Position.X, float32(sliderW), total))
			}

		case pointer.Release:
			a.sliderDragging = false

		case pointer.Scroll:
			a.handleSliderScroll(ptrEv.Scroll.Y, current, total)
		}
	}
}

// handleSliderScroll seeks by scrollY frames when the player is paused.
func (a *App) handleSliderScroll(scrollY float32, current, total int) {
	if a.player == nil || !a.player.IsPaused() {
		return
	}

	idx := current + int(scrollY)
	if idx < 0 {
		idx = 0
	} else if idx >= total {
		idx = total - 1
	}

	a.seekToFrame(idx)
}

func (a *App) layoutSlider(gtx layout.Context, current, total, _ int) layout.Dimensions {
	sliderH := gtx.Dp(unit.Dp(sliderHeightDp))
	trackH := gtx.Dp(unit.Dp(sliderTrackHeightDp))
	sliderW := gtx.Constraints.Max.X
	if sliderW < 1 {
		sliderW = 1
	}

	// Slider track area — register pointer events
	area := clip.Rect{Max: image.Pt(sliderW, sliderH)}.Push(gtx.Ops)
	event.Op(gtx.Ops, &a.sliderTag)
	area.Pop()

	a.handleSliderEvents(gtx, sliderW, total, current)

	// Draw track background
	trackY := (sliderH - trackH) / centerDiv
	{
		trackOp := clip.Rect{Min: image.Pt(0, trackY), Max: image.Pt(sliderW, trackY+trackH)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 80, G: 80, B: 80, A: 255})
		trackOp.Pop()
	}

	// Draw filled portion
	frac := float32(current) / float32(total)
	filledW := int(frac * float32(sliderW))
	{
		fillOp := clip.Rect{Min: image.Pt(0, trackY), Max: image.Pt(filledW, trackY+trackH)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 80, G: 140, B: 220, A: 255})
		fillOp.Pop()
	}

	// Draw thumb
	thumbW := gtx.Dp(unit.Dp(sliderThumbWidthDp))
	thumbX := filledW - thumbW/centerDiv
	if thumbX < 0 {
		thumbX = 0
	}
	{
		thumbOp := clip.Rect{Min: image.Pt(thumbX, 0), Max: image.Pt(thumbX+thumbW, sliderH)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 160, G: 200, B: 255, A: 255})
		thumbOp.Pop()
	}

	return layout.Dimensions{Size: image.Pt(sliderW, sliderH)}
}

func (a *App) sliderPosToFrame(x float32, width float32, total int) int {
	if width <= 0 {
		return 0
	}
	frac := x / width
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	idx := int(frac * float32(total))
	if idx >= total {
		idx = total - 1
	}

	return idx
}

func (a *App) seekToFrame(idx int) {
	if a.player == nil {
		return
	}
	// Cancel any running backfill immediately (non-blocking cancel + async wait)
	if a.playBuf != nil {
		a.playBuf.StopBackfill()
		a.playBuf.Clear()
	}
	// Cancel any pending debounced backfill
	if a.backfillTimer != nil {
		a.backfillTimer.Stop()
	}

	frame, err := a.player.SeekTo(idx)
	if err != nil {
		log.Printf("seek: %v", err)

		return
	}

	// Display the seeked frame immediately
	go a.UpdateFrame(frame)

	// Debounce backfill: wait 150ms after last seek before starting.
	// This avoids spawning workers on every slider drag pixel.
	if a.playBuf != nil {
		a.backfillTimer = time.AfterFunc(backfillDebounce, func() {
			a.mu.Lock()
			params := a.params
			rot := a.rotation
			a.mu.Unlock()
			a.playBuf.StartBackfill(a.player, idx, params, rot, a.invalidateGraphs)
		})
	}
}

// invalidateGraphs triggers a redraw of all open graph windows and the main window.
func (a *App) invalidateGraphs() {
	a.mu.Lock()
	for _, gw := range a.graphs {
		if !gw.IsClosed() {
			gw.Invalidate()
		}
	}
	a.mu.Unlock()
	a.Window.Invalidate()
}

func (a *App) layoutStatus(gtx layout.Context, _ *colorize.Result) layout.Dimensions {
	a.mu.Lock()
	params := a.params
	eIdx := a.emissivityIdx
	a.mu.Unlock()

	gainStr := "High"
	if a.gainMode == camera.GainLow {
		gainStr = "Low"
	}

	preset := colorize.EmissivityPresets[eIdx]
	epsLabel := fmt.Sprintf(" e: %.2f %s ", preset.Emissivity, preset.Name)

	// Recording/playback indicator
	recStr := ""
	if a.recorder != nil {
		recStr = fmt.Sprintf("  |  REC %d  %s", a.recorder.Frames(), humanSize(a.recorder.FileSize()))
	}

	// Buffer status — clickable button
	bufLabel := ""
	if a.playBuf != nil {
		bufLabel = fmt.Sprintf(" BUF %d/%d ", a.playBuf.Len(), a.playBuf.MaxLen())
	} else if a.frameBuf != nil {
		bufLabel = fmt.Sprintf(" BUF %d/%d ", a.frameBuf.Len(), a.frameBuf.maxFrames)
	}

	// Handle buffer size button click
	if a.bufClick.Clicked(gtx) {
		a.bufPanel.Toggle()
	}

	leftStatus := fmt.Sprintf("[C] %-10s  |  [A] %-10s  |  [G] Gain: %-4s  |",
		params.Palette, agcName(params.Mode), gainStr)
	rightStatus := fmt.Sprintf("|  [R] %d\u00b0  |  [H] Help%s", a.rotation*degreesPerRotStep, recStr)

	lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}

	// Handle emissivity button click
	if a.epsClick.Clicked(gtx) {
		a.epsDropdown.Toggle(eIdx)
	}

	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
			paint.Fill(gtx.Ops, color.NRGBA{R: 30, G: 30, B: 30, A: 255})

			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(statusBarInsetDp)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceStart}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceSides}.Layout(gtx,
							// Left section
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body2(a.theme, leftStatus)
								lbl.Color = lightGray

								return lbl.Layout(gtx)
							}),
							// Clickable emissivity button
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return dropdownButton(gtx, a.theme, &a.epsClick, a.epsDropdown.IsOpen(), epsLabel)
							}),
							// Right section
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body2(a.theme, rightStatus)
								lbl.Color = lightGray

								return lbl.Layout(gtx)
							}),
							// Clickable buffer size button
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if bufLabel == "" {
									return layout.Dimensions{}
								}

								return dropdownButton(gtx, a.theme, &a.bufClick, a.bufPanel.IsOpen(), bufLabel)
							}),
						)
					}),
				)
			})
		},
	)
}

func (a *App) layoutHelp(gtx layout.Context) {
	type row struct{ key, desc string }
	type section struct {
		title string
		rows  []row
	}
	sections := []section{
		{"Display", []row{
			{"C", "Cycle colormap"},
			{"A", "Cycle AGC mode"},
			{"V", "Toggle colorbar"},
			{"R", "Rotate 90\u00b0"},
			{"T", "Toggle temp labels"},
		}},
		{"Camera", []row{
			{"G", "Toggle gain (High/Low)"},
			{"S", "Trigger shutter/NUC"},
			{"E / Shift+E", "Cycle emissivity \u2192 / \u2190"},
		}},
		{"Measurement", []row{
			{"Click", "Place/remove point"},
			{"Shift+Click", "Select spot (for E)"},
			{"X", "Clear user points"},
			{"0-9", "Toggle graph for spot"},
		}},
		{"Recording & Export", []row{
			{"F12", "Save screenshot (PNG)"},
			{"D", "Dump raw frame (.tha)"},
			{"F5", "Start/stop recording (.tha)"},
		}},
		{"Playback", []row{
			{"Space", "Play/pause"},
			{"P", "Pause/resume"},
			{"Left/Right", "Step frame"},
		}},
		{"General", []row{
			{"H", "Toggle this help"},
			{"Q / Esc", "Quit"},
		}},
	}

	lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}
	dimGray := color.NRGBA{R: 160, G: 160, B: 160, A: 255}
	keyW := gtx.Dp(unit.Dp(keyHelpColumnWidthDp))

	// Semi-transparent background
	defer op.Offset(image.Pt(keyHelpPanelOffsetDp, keyHelpPanelOffsetDp)).Push(gtx.Ops).Pop()

	// Title + categorized rows
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(a.theme, "Keyboard Controls")
			lbl.Color = lightGray
			lbl.Font.Weight = font.Bold

			return layout.Inset{Bottom: unit.Dp(keyHelpTitleBottomDp)}.Layout(gtx, lbl.Layout)
		}),
	}

	for i, sec := range sections {
		isFirst := i == 0
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			topPad := unit.Dp(keyHelpSectionTopDp)
			if isFirst {
				topPad = unit.Dp(sectionTopPad0Dp)
			}

			return layout.Inset{Top: topPad, Bottom: unit.Dp(sectionBotPadDp)}.
				Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(a.theme, sec.title)
					lbl.Color = dimGray
					lbl.Font.Weight = font.Bold

					return lbl.Layout(gtx)
				})
		}))
		for _, row := range sec.rows {
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = keyW
						gtx.Constraints.Max.X = keyW
						lbl := material.Body2(a.theme, row.key)
						lbl.Color = lightGray

						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(a.theme, row.desc)
						lbl.Color = lightGray

						return lbl.Layout(gtx)
					}),
				)
			}))
		}
	}

	// Measure content first, then draw background behind it
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Max.X = keyW + gtx.Dp(unit.Dp(keyHelpExtraWidthDp))
	gtx.Constraints.Min = image.Point{}
	dims := layout.UniformInset(unit.Dp(keyHelpPanelInsetDp)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
	call := macro.Stop()

	// Background
	defer clip.Rect{Max: dims.Size}.Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, color.NRGBA{A: 200})

	// Replay content
	call.Add(gtx.Ops)
}

func (a *App) layoutToast(gtx layout.Context, msg string) {
	// Measure text to get natural size
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	dims := layout.UniformInset(unit.Dp(toastPaddingDp)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Body2(a.theme, msg)
		lbl.Color = color.NRGBA{R: 220, G: 220, B: 220, A: 255}

		return lbl.Layout(gtx)
	})
	call := macro.Stop()

	// Center horizontally, near bottom
	x := (gtx.Constraints.Max.X - dims.Size.X) / centerDiv
	y := gtx.Constraints.Max.Y - gtx.Dp(unit.Dp(toastBottomOffsetDp))
	defer op.Offset(image.Pt(x, y)).Push(gtx.Ops).Pop()

	defer clip.Rect{Max: dims.Size}.Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, color.NRGBA{A: 200})
	call.Add(gtx.Ops)
}

func agcName(m colorize.AGCMode) string {
	switch m {
	case colorize.AGCHardware:
		return "HW AGC"
	case colorize.AGCPercentile:
		return "Percentile"
	case colorize.AGCFixed:
		return "Fixed"
	}

	return "?"
}

func humanSize(bytes int64) string {
	const (
		kilobyte = 1024
		megabyte = 1024 * kilobyte
		gigabyte = 1024 * megabyte
		terabyte = 1024 * gigabyte
	)
	switch {
	case bytes >= terabyte:
		return fmt.Sprintf("%.1f TB", float64(bytes)/float64(terabyte))
	case bytes >= gigabyte:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gigabyte))
	case bytes >= megabyte:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(megabyte))
	case bytes >= kilobyte:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kilobyte))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func (a *App) saveScreenshot(img *image.RGBA) {
	name := fmt.Sprintf("thermal_%s.png", time.Now().Format("20060102_150405"))
	outFile, err := os.Create(name)
	if err != nil {
		log.Printf("screenshot: %v", err)

		return
	}
	defer outFile.Close()
	if err := png.Encode(outFile, img); err != nil {
		log.Printf("screenshot encode: %v", err)

		return
	}
	log.Printf("saved screenshot: %s", name)

	a.mu.Lock()
	a.toastMsg = fmt.Sprintf("Screenshot saved: %s", name)
	a.toastExpiry = time.Now().Add(toastDuration)
	a.mu.Unlock()
	a.Window.Invalidate()
}

func (a *App) dumpFrame(frame *camera.Frame) {
	name := fmt.Sprintf("thermal_%s.tha", time.Now().Format("20060102_150405"))
	if err := recording.DumpFrame(name, frame); err != nil {
		log.Printf("frame dump: %v", err)

		return
	}
	log.Printf("dumped raw frame: %s", name)

	a.mu.Lock()
	a.toastMsg = fmt.Sprintf("Frame dumped: %s", name)
	a.toastExpiry = time.Now().Add(toastDuration)
	a.mu.Unlock()
	a.Window.Invalidate()
}

func (a *App) toggleRecording() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.recorder != nil {
		// Stop recording
		frames := a.recorder.Frames()
		if err := a.recorder.Close(); err != nil {
			log.Printf("stop recording: %v", err)
		}
		a.recorder = nil
		a.toastMsg = fmt.Sprintf("Recording stopped (%d frames)", frames)
		a.toastExpiry = time.Now().Add(toastDuration)

		return
	}

	// Start recording
	size := a.cam.SensorSize()
	name := fmt.Sprintf("thermal_%s.tha", time.Now().Format("20060102_150405"))
	rec, err := recording.NewRecorder(name, size.X, size.Y)
	if err != nil {
		log.Printf("start recording: %v", err)
		a.toastMsg = fmt.Sprintf("Recording failed: %v", err)
		a.toastExpiry = time.Now().Add(toastDuration)

		return
	}
	a.recorder = rec
	a.toastMsg = fmt.Sprintf("Recording: %s", name)
	a.toastExpiry = time.Now().Add(toastDuration)
	log.Printf("recording started: %s", name)
}

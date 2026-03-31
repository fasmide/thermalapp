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

// App holds the UI and shared state.
type App struct {
	Window *app.Window
	theme  *material.Theme

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

func NewApp(cam camera.Camera, bufSize int64) *App {
	size := cam.SensorSize()
	var w app.Window
	title := "P3 Thermal"
	w.Option(
		app.Title(title),
		app.Size(unit.Dp(float32(size.X*3)), unit.Dp(float32(size.Y*3+80))),
	)
	return &App{
		Window: &w,
		theme:  material.NewTheme(),
		cam:    cam,
		params: colorize.Params{
			Mode:       colorize.AGCPercentile,
			Palette:    colorize.PaletteInferno,
			Emissivity: colorize.DefaultEmissivity,
		},
		spots: []*Spot{
			NewSpot(0, SpotMin, color.NRGBA{R: 60, G: 120, B: 255, A: 230}),
			NewSpot(1, SpotMax, color.NRGBA{R: 255, G: 60, B: 60, A: 230}),
			NewSpot(2, SpotCursor, color.NRGBA{R: 180, G: 180, B: 180, A: 200}),
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
	a.Window.Option(app.Title(fmt.Sprintf("P3 Thermal — Playback (%d frames)", p.FrameCount())))
}

func (a *App) UpdateFrame(frame *camera.Frame) {
	a.mu.Lock()
	p := a.params
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

	result := colorize.Colorize(frame, p).Rotate(rot)

	a.mu.Lock()
	a.result = result
	a.mu.Unlock()

	imgW := result.RGBA.Bounds().Dx()

	// Add to the appropriate buffer
	if a.playBuf != nil {
		// Playback mode: cache by frame index
		frameIdx := a.player.FrameIndex() - 1
		if frameIdx < 0 {
			frameIdx = 0
		}
		if bw, bh := a.playBuf.Dims(); bw != imgW || bh != result.RGBA.Bounds().Dy() {
			a.playBuf.Resize(imgW, result.RGBA.Bounds().Dy())
		}
		a.playBuf.Add(frameIdx, result.Celsius, a.player.FrameTime())
	} else {
		// Live mode: ring buffer
		if bw, bh := a.frameBuf.Dims(); bw != imgW || bh != result.RGBA.Bounds().Dy() {
			a.frameBuf.Resize(imgW, result.RGBA.Bounds().Dy())
		}
		a.frameBuf.Add(result.Celsius, time.Now())
	}

	// Update spot positions and record temperatures
	const alpha = 0.15

	// Spot 0: min
	minSpot := a.spots[0]
	newMinX, newMinY := float32(result.MinX), float32(result.MinY)
	if !a.smInited.Load() {
		minSpot.SetPosition(newMinX, newMinY, true)
	} else {
		minSpot.UpdateEMA(newMinX, newMinY, alpha)
	}
	mx, my := minSpot.GetPosition()
	minIdx := int(my)*imgW + int(mx)
	if minIdx >= 0 && minIdx < len(result.Celsius) {
		minSpot.Record(result.Celsius[minIdx])
	}

	// Spot 1: max
	maxSpot := a.spots[1]
	newMaxX, newMaxY := float32(result.MaxX), float32(result.MaxY)
	if !a.smInited.Load() {
		maxSpot.SetPosition(newMaxX, newMaxY, true)
	} else {
		maxSpot.UpdateEMA(newMaxX, newMaxY, alpha)
	}
	mxx, mxy := maxSpot.GetPosition()
	maxIdx := int(mxy)*imgW + int(mxx)
	if maxIdx >= 0 && maxIdx < len(result.Celsius) {
		maxSpot.Record(result.Celsius[maxIdx])
	}

	a.smInited.Store(true)

	// Spot 2: cursor — recorded in handlePointer, just read temp here if active
	cursorSpot := a.spots[2]
	cs := cursorSpot.GetState()
	if cs.Active {
		cIdx := int(cs.Y)*imgW + int(cs.X)
		if cIdx >= 0 && cIdx < len(result.Celsius) {
			cursorSpot.SetLastTemp(result.Celsius[cIdx])
		}
	}

	// User spots (3+)
	a.mu.Lock()
	userSpots := make([]*Spot, len(a.spots[3:]))
	copy(userSpots, a.spots[3:])
	a.mu.Unlock()
	for _, sp := range userSpots {
		st := sp.GetState()
		idx := int(st.Y)*imgW + int(st.X)
		if idx >= 0 && idx < len(result.Celsius) {
			temp := sp.CorrectedTemp(result.Celsius[idx], result.GlobalEmissivity, result.AmbientC)
			sp.SetLastTemp(temp)
		}
	}

	// Invalidate graph windows
	for _, gw := range a.graphs {
		if !gw.IsClosed() {
			gw.Invalidate()
		}
	}

	a.Window.Invalidate()
}

// refreshDisplay re-colorizes the last raw frame with the current params.
// Call after changing palette, emissivity, AGC mode, or rotation while paused.
func (a *App) refreshDisplay() {
	a.mu.Lock()
	frame := a.lastFrame
	p := a.params
	rot := a.rotation
	a.mu.Unlock()

	if frame == nil {
		return
	}

	result := colorize.Colorize(frame, p).Rotate(rot)

	a.mu.Lock()
	a.result = result
	a.mu.Unlock()

	imgW := result.RGBA.Bounds().Dx()

	// Re-read spot temperatures at their current positions.
	for i, sp := range a.spots {
		st := sp.GetState()
		if !st.Active {
			continue
		}
		idx := int(st.Y)*imgW + int(st.X)
		if idx < 0 || idx >= len(result.Celsius) {
			continue
		}
		if i >= 3 {
			temp := sp.CorrectedTemp(result.Celsius[idx], result.GlobalEmissivity, result.AmbientC)
			sp.SetLastTemp(temp)
		} else {
			sp.SetLastTemp(result.Celsius[idx])
		}
	}

	a.Window.Invalidate()
}

func (a *App) Run() error {
	var ops op.Ops
	for {
		switch e := a.Window.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			// Fill window background black
			paint.Fill(gtx.Ops, color.NRGBA{A: 255})
			a.handleKeys(gtx)
			a.doLayout(gtx)
			e.Frame(gtx.Ops)
		}
	}
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
		ev, ok := gtx.Source.Event(filters...)
		if !ok {
			break
		}
		ke, ok := ev.(key.Event)
		if !ok || ke.State != key.Press {
			continue
		}

		switch ke.Name {
		case "Q", key.NameEscape:
			if ke.Name == key.NameEscape && a.epsDropdown.IsOpen() {
				a.epsDropdown.Close()
			} else if ke.Name == key.NameEscape && a.bufPanel.IsOpen() {
				a.bufPanel.Close()
			} else if ke.Name == key.NameEscape && a.selectedSpot >= 0 {
				a.selectedSpot = -1
			} else {
				a.Window.Perform(system.ActionClose)
			}

		case "C":
			a.mu.Lock()
			a.params.Palette = a.params.Palette.Next()
			a.mu.Unlock()
			a.refreshDisplay()

		case "A":
			a.mu.Lock()
			switch a.params.Mode {
			case colorize.AGCPercentile:
				a.params.Mode = colorize.AGCHardware
			case colorize.AGCHardware:
				a.params.Mode = colorize.AGCPercentile
			default:
				a.params.Mode = colorize.AGCPercentile
			}
			a.mu.Unlock()
			a.refreshDisplay()

		case "G":
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

		case "S":
			go func() {
				if err := a.cam.TriggerShutter(); err != nil {
					log.Printf("shutter: %v", err)
				}
			}()

		case "V":
			a.showColorbar = !a.showColorbar

		case "H":
			a.showHelp = !a.showHelp

		case "R":
			a.rotation = (a.rotation + 1) % 4
			a.refreshDisplay()

		case "T":
			a.showLabels = !a.showLabels

		case "E":
			backward := ke.Modifiers.Contain(key.ModShift)
			nPresets := len(colorize.EmissivityPresets)
			a.mu.Lock()
			if a.selectedSpot >= 0 && a.selectedSpot < len(a.spots) {
				sp := a.spots[a.selectedSpot]
				_, curIdx := sp.GetEmissivity()
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
					sp.SetEmissivity(0, -1)
					a.toastMsg = fmt.Sprintf("Spot %d: ε = global", a.selectedSpot)
				} else {
					preset := colorize.EmissivityPresets[curIdx]
					sp.SetEmissivity(preset.Emissivity, curIdx)
					a.toastMsg = fmt.Sprintf("Spot %d: ε = %.2f  %s", a.selectedSpot, preset.Emissivity, preset.Name)
				}
			} else {
				if backward {
					a.emissivityIdx = (a.emissivityIdx - 1 + nPresets) % nPresets
				} else {
					a.emissivityIdx = (a.emissivityIdx + 1) % nPresets
				}
				preset := colorize.EmissivityPresets[a.emissivityIdx]
				a.params.Emissivity = preset.Emissivity
				a.toastMsg = fmt.Sprintf("ε = %.2f  %s", preset.Emissivity, preset.Name)
			}
			a.toastExpiry = time.Now().Add(2 * time.Second)
			a.mu.Unlock()
			a.refreshDisplay()

		case "X":
			a.mu.Lock()
			// Close any graph windows for user spots (index >= 3)
			for idx, gw := range a.graphs {
				if idx >= 3 {
					gw.mu.Lock()
					gw.window.Perform(system.ActionClose)
					gw.mu.Unlock()
					delete(a.graphs, idx)
				}
			}
			a.spots = a.spots[:3] // keep min, max, cursor
			a.selectedSpot = -1
			a.mu.Unlock()

		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(ke.Name[0] - '0')
			a.toggleGraph(idx)

		case "D":
			a.mu.Lock()
			frame := a.lastFrame
			a.mu.Unlock()
			if frame != nil {
				go a.dumpFrame(frame)
			}

		case key.NameF5:
			a.toggleRecording()

		case "P":
			if a.player != nil {
				paused := a.player.IsPaused()
				a.player.SetPaused(!paused)
				a.mu.Lock()
				if !paused {
					a.toastMsg = "Playback paused"
				} else {
					a.toastMsg = "Playback resumed"
				}
				a.toastExpiry = time.Now().Add(2 * time.Second)
				a.mu.Unlock()
			}

		case key.NameLeftArrow:
			if a.player != nil {
				idx := a.player.FrameIndex() - 2
				if idx < 0 {
					idx = 0
				}
				a.seekToFrame(idx)
			}

		case key.NameRightArrow:
			if a.player != nil {
				idx := a.player.FrameIndex()
				a.seekToFrame(idx)
			}

		case key.NameSpace:
			if a.player != nil {
				paused := a.player.IsPaused()
				a.player.SetPaused(!paused)
				a.mu.Lock()
				if !paused {
					a.toastMsg = "Playback paused"
				} else {
					a.toastMsg = "Playback resumed"
				}
				a.toastExpiry = time.Now().Add(2 * time.Second)
				a.mu.Unlock()
			}

		case key.NameF12:
			a.mu.Lock()
			r := a.result
			a.mu.Unlock()
			if r != nil && r.RGBA != nil {
				go a.saveScreenshot(r.RGBA)
			}
		}
	}
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
		ev, ok := gtx.Source.Event(filters...)
		if !ok {
			break
		}
		pe, ok := ev.(pointer.Event)
		if !ok {
			continue
		}

		switch pe.Kind {
		case pointer.Move, pointer.Enter:
			a.cursorPos = pe.Position
			// Map from screen coords to image pixel coords
			imgX := int((pe.Position.X - float32(a.imgOffsetX)) / a.imgScale)
			imgY := int((pe.Position.Y - float32(a.imgOffsetY)) / a.imgScale)

			a.mu.Lock()
			r := a.result
			a.mu.Unlock()

			cursorSpot := a.spots[2]
			if r != nil && imgX >= 0 && imgY >= 0 && imgX < r.RGBA.Bounds().Dx() && imgY < r.RGBA.Bounds().Dy() {
				cursorSpot.SetPosition(float32(imgX), float32(imgY), true)
				// Update cursor temperature immediately so the label reflects the current pixel
				imgW := r.RGBA.Bounds().Dx()
				cIdx := imgY*imgW + imgX
				if cIdx >= 0 && cIdx < len(r.Celsius) {
					cursorSpot.SetLastTemp(r.Celsius[cIdx])
				}
			} else {
				cursorSpot.SetActive(false)
			}
			// Invalidate cursor graph so it rebuilds from buffer at new position
			a.mu.Lock()
			cursorGW := a.graphs[2]
			a.mu.Unlock()
			if cursorGW != nil && !cursorGW.IsClosed() {
				cursorGW.Invalidate()
			}
		case pointer.Press:
			// Map screen to image coords
			imgX := int((pe.Position.X - float32(a.imgOffsetX)) / a.imgScale)
			imgY := int((pe.Position.Y - float32(a.imgOffsetY)) / a.imgScale)

			a.mu.Lock()
			r := a.result
			a.mu.Unlock()

			if r != nil && imgX >= 0 && imgY >= 0 && imgX < r.RGBA.Bounds().Dx() && imgY < r.RGBA.Bounds().Dy() {
				// Shift+Click: select/deselect a spot for per-spot emissivity
				if pe.Modifiers.Contain(key.ModShift) {
					a.mu.Lock()
					found := -1
					for i := 3; i < len(a.spots); i++ {
						spX, spY := a.spots[i].GetPosition()
						dx := int(spX) - imgX
						dy := int(spY) - imgY
						if dx*dx+dy*dy < 25 {
							found = i
							break
						}
					}
					if found >= 0 {
						if a.selectedSpot == found {
							a.selectedSpot = -1 // deselect
						} else {
							a.selectedSpot = found
						}
					}
					a.mu.Unlock()
				} else {
					// Normal click: add or remove a user measurement point
					removed := false
					a.mu.Lock()
					for i := 3; i < len(a.spots); i++ {
						spX, spY := a.spots[i].GetPosition()
						dx := int(spX) - imgX
						dy := int(spY) - imgY
						if dx*dx+dy*dy < 25 {
							// Deselect if this was the selected spot
							if a.selectedSpot == i {
								a.selectedSpot = -1
							} else if a.selectedSpot > i {
								a.selectedSpot--
							}
							// Close graph window if open
							if gw, ok := a.graphs[i]; ok {
								gw.mu.Lock()
								gw.window.Perform(system.ActionClose)
								gw.mu.Unlock()
								delete(a.graphs, i)
							}
							a.spots = append(a.spots[:i], a.spots[i+1:]...)
							// Re-number remaining user spots and fix graph map keys
							newGraphs := make(map[int]*GraphWindow)
							for k, v := range a.graphs {
								if k < 3 {
									newGraphs[k] = v
								}
							}
							for j := 3; j < len(a.spots); j++ {
								a.spots[j].Index = j
								if gw, ok := a.graphs[j+1]; ok {
									newGraphs[j] = gw
								}
							}
							a.graphs = newGraphs
							removed = true
							break
						}
					}
					if !removed {
						idx := len(a.spots)
						sp := NewSpot(idx, SpotUser, color.NRGBA{R: 60, G: 220, B: 60, A: 230})
						sp.SetPosition(float32(imgX), float32(imgY), true)
						a.spots = append(a.spots, sp)
					}
					a.mu.Unlock()
				}
			}

		case pointer.Leave:
			a.spots[2].SetActive(false)
		}
	}
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

	if a.showHelp {
		a.layoutHelp(gtx)
	}

	// Emissivity dropdown overlay
	if a.epsDropdown.IsOpen() {
		a.mu.Lock()
		eIdx := a.emissivityIdx
		a.mu.Unlock()
		if sel := a.epsDropdown.Layout(gtx, a.theme, eIdx); sel >= 0 {
			a.mu.Lock()
			a.emissivityIdx = sel
			preset := colorize.EmissivityPresets[sel]
			a.params.Emissivity = preset.Emissivity
			a.toastMsg = fmt.Sprintf("ε = %.2f  %s", preset.Emissivity, preset.Name)
			a.toastExpiry = time.Now().Add(2 * time.Second)
			a.mu.Unlock()
			a.refreshDisplay()
		}
	}

	// Buffer settings panel overlay
	if a.bufPanel.IsOpen() {
		curBytes := a.frameBuf.MaxBytes()
		curInterval := a.frameBuf.SampleInterval()
		w, h := a.frameBuf.Dims()
		res := a.bufPanel.Layout(gtx, a.theme, curBytes, curInterval, 4, w*h)
		if res.SizeChanged {
			a.frameBuf.SetMaxBytes(res.NewBytes)
			if a.playBuf != nil {
				a.playBuf.StopBackfill()
				a.playBuf.SetMaxBytes(res.NewBytes)
				idx := a.player.FrameIndex()
				a.mu.Lock()
				params := a.params
				rot := a.rotation
				a.mu.Unlock()
				a.playBuf.StartBackfill(a.player, idx, params, rot, a.invalidateGraphs)
			}
			a.mu.Lock()
			a.toastMsg = fmt.Sprintf("Buffer resized to %s", humanSize(res.NewBytes))
			a.toastExpiry = time.Now().Add(2 * time.Second)
			a.mu.Unlock()
		}
		if res.IntervalChanged {
			a.frameBuf.SetSampleInterval(res.NewInterval)
			if a.playBuf != nil {
				// Convert interval to frame skip (recording is ~25fps)
				skip := 1
				if res.NewInterval > 0 {
					skip = int(res.NewInterval.Milliseconds() / 40) // 40ms per frame at 25fps
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
				if p.Interval == res.NewInterval {
					label = p.Label
					break
				}
			}
			a.mu.Lock()
			a.toastMsg = fmt.Sprintf("Sample rate: %s", label)
			a.toastExpiry = time.Now().Add(2 * time.Second)
			a.mu.Unlock()
		}
	}

	// Toast overlay
	a.mu.Lock()
	msg := a.toastMsg
	expiry := a.toastExpiry
	a.mu.Unlock()
	if msg != "" && time.Now().Before(expiry) {
		a.layoutToast(gtx, msg)
		a.Window.Invalidate()
	}

	return dims
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

	offsetX := (int(availW) - scaledW) / 2
	offsetY := (int(availH) - scaledH) / 2

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
		s1 := op.Offset(image.Pt(offsetX, offsetY)).Push(gtx.Ops)
		s2 := clip.Rect{Max: image.Pt(scaledW, scaledH)}.Push(gtx.Ops)
		aff := f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(scale, scale))
		s3 := op.Affine(aff).Push(gtx.Ops)

		imgOp := paint.NewImageOp(img)
		imgOp.Filter = paint.FilterNearest
		imgOp.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)

		s3.Pop()
		s2.Pop()
		s1.Pop()
	}

	// Draw spot markers and labels
	markerSize := 4
	a.mu.Lock()
	allSpots := make([]*Spot, len(a.spots))
	copy(allSpots, a.spots)
	selIdx := a.selectedSpot
	a.mu.Unlock()

	// Take a snapshot of each spot's mutable state (thread-safe)
	type spotSnap struct {
		sp    *Spot
		state SpotState
	}
	snaps := make([]spotSnap, len(allSpots))
	for i, sp := range allSpots {
		snaps[i] = spotSnap{sp: sp, state: sp.GetState()}
	}

	for _, sn := range snaps {
		if !sn.state.Active {
			continue
		}
		if sn.sp.Kind == SpotCursor {
			continue
		}

		cx := offsetX + int(sn.state.X*scale+scale/2)
		cy := offsetY + int(sn.state.Y*scale+scale/2)

		// Selection highlight: larger yellow ring
		if sn.sp.Index == selIdx {
			ringSize := markerSize + 3
			s := clip.Rect{Min: image.Pt(cx-ringSize, cy-ringSize), Max: image.Pt(cx+ringSize, cy+ringSize)}.Push(gtx.Ops)
			paint.Fill(gtx.Ops, color.NRGBA{R: 255, G: 220, B: 0, A: 255})
			s.Pop()
		}

		mx := cx - markerSize
		my := cy - markerSize
		sz := markerSize * 2
		s := clip.Rect{Min: image.Pt(mx, my), Max: image.Pt(mx+sz, my+sz)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, sn.sp.Color)
		s.Pop()
	}

	// Temperature labels
	if a.showLabels {
		imgW := result.RGBA.Bounds().Dx()
		for _, sn := range snaps {
			if !sn.state.Active || sn.sp.Kind == SpotCursor {
				continue
			}
			idx := int(sn.state.Y)*imgW + int(sn.state.X)
			if idx >= 0 && idx < len(result.Celsius) {
				lx := offsetX + int(sn.state.X*scale+scale/2)
				ly := offsetY + int(sn.state.Y*scale) - 2
				temp := sn.sp.CorrectedTemp(result.Celsius[idx], result.GlobalEmissivity, result.AmbientC)
				epsSuffix := ""
				if sn.state.Emissivity > 0 {
					epsSuffix = fmt.Sprintf(" e%.2f", sn.state.Emissivity)
				}
				a.drawSpotLabel(gtx, lx, ly, sn.sp.Index, temp, epsSuffix, sn.sp.Color)
			}
		}
	}

	// Cursor temperature label (next to mouse pointer)
	cursorSpot := a.spots[2]
	cursorState := snaps[2].state
	if cursorState.Active {
		cx := int(a.cursorPos.X) + 12
		cy := int(a.cursorPos.Y) - 6
		a.drawTempLabel(gtx, cx, cy, cursorSpot.LastTemp(), cursorSpot.Color)
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
func (a *App) drawSpotLabel(gtx layout.Context, sx, sy int, index int, temp float32, suffix string, col color.NRGBA) {
	txt := fmt.Sprintf("[%d] %.1f\u00b0%s", index, temp, suffix)

	// Measure
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	dims := layout.Inset{Left: unit.Dp(3), Right: unit.Dp(3), Top: unit.Dp(1), Bottom: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(a.theme, txt)
		lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
		return lbl.Layout(gtx)
	})
	call := macro.Stop()

	ox := sx - dims.Size.X/2
	oy := sy - dims.Size.Y
	s := op.Offset(image.Pt(ox, oy)).Push(gtx.Ops)

	pill := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
	paint.Fill(gtx.Ops, col)
	pill.Pop()

	call.Add(gtx.Ops)
	s.Pop()
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
func (a *App) drawTempLabel(gtx layout.Context, sx, sy int, temp float32, col color.NRGBA) {
	txt := fmt.Sprintf("%.1f\u00b0", temp)

	// Measure
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	dims := layout.Inset{Left: unit.Dp(3), Right: unit.Dp(3), Top: unit.Dp(1), Bottom: unit.Dp(1)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(a.theme, txt)
		lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
		return lbl.Layout(gtx)
	})
	call := macro.Stop()

	// Position: center horizontally above the marker
	ox := sx - dims.Size.X/2
	oy := sy - dims.Size.Y
	s := op.Offset(image.Pt(ox, oy)).Push(gtx.Ops)

	// Background pill
	pill := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
	paint.Fill(gtx.Ops, col)
	pill.Pop()

	call.Add(gtx.Ops)
	s.Pop()
}

// drawNUCIndicator draws a "NUC" badge at the top-right of the image area.
func (a *App) drawNUCIndicator(gtx layout.Context, rightX, topY int) {
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Min = image.Point{}
	dims := layout.Inset{Left: unit.Dp(6), Right: unit.Dp(6), Top: unit.Dp(3), Bottom: unit.Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Body1(a.theme, "NUC")
		lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
		lbl.Font.Weight = font.Bold
		return lbl.Layout(gtx)
	})
	call := macro.Stop()

	ox := rightX - dims.Size.X - gtx.Dp(unit.Dp(8))
	oy := topY + gtx.Dp(unit.Dp(8))
	s := op.Offset(image.Pt(ox, oy)).Push(gtx.Ops)

	bg := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
	paint.Fill(gtx.Ops, color.NRGBA{R: 200, G: 40, B: 40, A: 220})
	bg.Pop()

	call.Add(gtx.Ops)
	s.Pop()
}

func (a *App) layoutColorbar(gtx layout.Context, result *colorize.Result) layout.Dimensions {
	barH := gtx.Dp(unit.Dp(20))
	barImg := colorize.MakeColorbar(a.params.Palette, barH)

	// Scale colorbar to full width
	barW := gtx.Constraints.Max.X
	scaleX := float32(barW) / float32(barImg.Bounds().Dx())
	scaleY := float32(barH) / float32(barImg.Bounds().Dy())

	// Draw the gradient bar
	{
		s1 := clip.Rect{Max: image.Pt(barW, barH)}.Push(gtx.Ops)
		aff := f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(scaleX, scaleY))
		s2 := op.Affine(aff).Push(gtx.Ops)

		imgOp := paint.NewImageOp(barImg)
		imgOp.Filter = paint.FilterLinear
		imgOp.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)

		s2.Pop()
		s1.Pop()
	}

	// Pick contrasting text colors based on LUT endpoints
	lut := a.params.Palette.LUT()
	minBg := lut[0]
	maxBg := lut[255]
	contrastColor := func(bg [3]uint8) color.NRGBA {
		// Perceived luminance
		lum := 0.299*float32(bg[0]) + 0.587*float32(bg[1]) + 0.114*float32(bg[2])
		if lum > 128 {
			return color.NRGBA{A: 255} // black
		}
		return color.NRGBA{R: 255, G: 255, B: 255, A: 255} // white
	}

	// Min label (left)
	{
		s := op.Offset(image.Pt(4, 1)).Push(gtx.Ops)
		lbl := material.Caption(a.theme, fmt.Sprintf("%.1f\u00b0C", result.MinC))
		lbl.Color = contrastColor(minBg)
		lbl.Layout(gtx)
		s.Pop()
	}

	// Max label (right)
	{
		maxStr := fmt.Sprintf("%.1f\u00b0C", result.MaxC)
		s := op.Offset(image.Pt(barW-gtx.Dp(unit.Dp(55)), 1)).Push(gtx.Ops)
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

	barH := gtx.Dp(unit.Dp(28))
	lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}
	bgColor := color.NRGBA{R: 35, G: 35, B: 35, A: 255}

	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
			paint.Fill(gtx.Ops, bgColor)
			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(2)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
									return layout.Inset{Left: unit.Dp(8), Right: unit.Dp(8), Top: unit.Dp(2), Bottom: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
						return layout.Inset{Left: unit.Dp(6), Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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

func (a *App) layoutSlider(gtx layout.Context, current, total, barH int) layout.Dimensions {
	sliderH := gtx.Dp(unit.Dp(12))
	trackH := gtx.Dp(unit.Dp(4))
	w := gtx.Constraints.Max.X
	if w < 1 {
		w = 1
	}

	// Slider track area — register pointer events
	area := clip.Rect{Max: image.Pt(w, sliderH)}.Push(gtx.Ops)
	event.Op(gtx.Ops, &a.sliderTag)
	area.Pop()

	// Handle pointer events (click, drag, scroll)
	filters := []event.Filter{
		pointer.Filter{
			Target:  &a.sliderTag,
			Kinds:   pointer.Press | pointer.Drag | pointer.Release | pointer.Scroll,
			ScrollY: pointer.ScrollRange{Min: -10, Max: 10},
		},
	}
	for {
		ev, ok := gtx.Source.Event(filters...)
		if !ok {
			break
		}
		pe, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch pe.Kind {
		case pointer.Press:
			a.sliderDragging = true
			idx := a.sliderPosToFrame(pe.Position.X, float32(w), total)
			a.seekToFrame(idx)
		case pointer.Drag:
			if a.sliderDragging {
				idx := a.sliderPosToFrame(pe.Position.X, float32(w), total)
				a.seekToFrame(idx)
			}
		case pointer.Release:
			a.sliderDragging = false
		case pointer.Scroll:
			if a.player != nil && a.player.IsPaused() {
				skip := int(pe.Scroll.Y)
				idx := current + skip
				if idx < 0 {
					idx = 0
				}
				if idx >= total {
					idx = total - 1
				}
				a.seekToFrame(idx)
			}
		}
	}

	// Draw track background
	trackY := (sliderH - trackH) / 2
	{
		s := clip.Rect{Min: image.Pt(0, trackY), Max: image.Pt(w, trackY+trackH)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 80, G: 80, B: 80, A: 255})
		s.Pop()
	}

	// Draw filled portion
	frac := float32(current) / float32(total)
	filledW := int(frac * float32(w))
	{
		s := clip.Rect{Min: image.Pt(0, trackY), Max: image.Pt(filledW, trackY+trackH)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 80, G: 140, B: 220, A: 255})
		s.Pop()
	}

	// Draw thumb
	thumbW := gtx.Dp(unit.Dp(8))
	thumbX := filledW - thumbW/2
	if thumbX < 0 {
		thumbX = 0
	}
	{
		s := clip.Rect{Min: image.Pt(thumbX, 0), Max: image.Pt(thumbX+thumbW, sliderH)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 160, G: 200, B: 255, A: 255})
		s.Pop()
	}

	return layout.Dimensions{Size: image.Pt(w, sliderH)}
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
		a.backfillTimer = time.AfterFunc(150*time.Millisecond, func() {
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

func (a *App) layoutStatus(gtx layout.Context, result *colorize.Result) layout.Dimensions {
	a.mu.Lock()
	p := a.params
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
		p.Palette, agcName(p.Mode), gainStr)
	rightStatus := fmt.Sprintf("|  [R] %d\u00b0  |  [H] Help%s", a.rotation*90, recStr)

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
			return layout.UniformInset(unit.Dp(4)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
								return material.Clickable(gtx, &a.epsClick, func(gtx layout.Context) layout.Dimensions {
									return layout.Background{}.Layout(gtx,
										func(gtx layout.Context) layout.Dimensions {
											bgCol := color.NRGBA{R: 50, G: 50, B: 50, A: 255}
											if a.epsClick.Hovered() {
												bgCol = color.NRGBA{R: 70, G: 70, B: 70, A: 255}
											}
											if a.epsDropdown.IsOpen() {
												bgCol = color.NRGBA{R: 60, G: 90, B: 160, A: 255}
											}
											defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
											paint.Fill(gtx.Ops, bgCol)
											return layout.Dimensions{Size: gtx.Constraints.Min}
										},
										func(gtx layout.Context) layout.Dimensions {
											return layout.Inset{Left: unit.Dp(6), Right: unit.Dp(6), Top: unit.Dp(2), Bottom: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
												lbl := material.Body2(a.theme, epsLabel)
												lbl.Color = lightGray
												return lbl.Layout(gtx)
											})
										},
									)
								})
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
								return material.Clickable(gtx, &a.bufClick, func(gtx layout.Context) layout.Dimensions {
									return layout.Background{}.Layout(gtx,
										func(gtx layout.Context) layout.Dimensions {
											bgCol := color.NRGBA{R: 50, G: 50, B: 50, A: 255}
											if a.bufClick.Hovered() {
												bgCol = color.NRGBA{R: 70, G: 70, B: 70, A: 255}
											}
											if a.bufPanel.IsOpen() {
												bgCol = color.NRGBA{R: 60, G: 90, B: 160, A: 255}
											}
											defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
											paint.Fill(gtx.Ops, bgCol)
											return layout.Dimensions{Size: gtx.Constraints.Min}
										},
										func(gtx layout.Context) layout.Dimensions {
											return layout.Inset{Left: unit.Dp(6), Right: unit.Dp(6), Top: unit.Dp(2), Bottom: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
												lbl := material.Body2(a.theme, bufLabel)
												lbl.Color = lightGray
												return lbl.Layout(gtx)
											})
										},
									)
								})
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
	keyW := gtx.Dp(unit.Dp(100))

	// Semi-transparent background
	defer op.Offset(image.Pt(20, 20)).Push(gtx.Ops).Pop()

	// Title + categorized rows
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(a.theme, "Keyboard Controls")
			lbl.Color = lightGray
			lbl.Font.Weight = font.Bold
			return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, lbl.Layout)
		}),
	}

	for i, sec := range sections {
		sec := sec
		isFirst := i == 0
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			topPad := unit.Dp(10)
			if isFirst {
				topPad = unit.Dp(2)
			}
			return layout.Inset{Top: topPad, Bottom: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(a.theme, sec.title)
				lbl.Color = dimGray
				lbl.Font.Weight = font.Bold
				return lbl.Layout(gtx)
			})
		}))
		for _, r := range sec.rows {
			r := r
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = keyW
						gtx.Constraints.Max.X = keyW
						lbl := material.Body2(a.theme, r.key)
						lbl.Color = lightGray
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(a.theme, r.desc)
						lbl.Color = lightGray
						return lbl.Layout(gtx)
					}),
				)
			}))
		}
	}

	// Measure content first, then draw background behind it
	macro := op.Record(gtx.Ops)
	gtx.Constraints.Max.X = keyW + gtx.Dp(unit.Dp(220))
	gtx.Constraints.Min = image.Point{}
	dims := layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
	dims := layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Body2(a.theme, msg)
		lbl.Color = color.NRGBA{R: 220, G: 220, B: 220, A: 255}
		return lbl.Layout(gtx)
	})
	call := macro.Stop()

	// Center horizontally, near bottom
	x := (gtx.Constraints.Max.X - dims.Size.X) / 2
	y := gtx.Constraints.Max.Y - gtx.Dp(unit.Dp(80))
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

func humanSize(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(TB))
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func (a *App) saveScreenshot(img *image.RGBA) {
	name := fmt.Sprintf("thermal_%s.png", time.Now().Format("20060102_150405"))
	f, err := os.Create(name)
	if err != nil {
		log.Printf("screenshot: %v", err)
		return
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Printf("screenshot encode: %v", err)
		return
	}
	log.Printf("saved screenshot: %s", name)

	a.mu.Lock()
	a.toastMsg = fmt.Sprintf("Screenshot saved: %s", name)
	a.toastExpiry = time.Now().Add(3 * time.Second)
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
	a.toastExpiry = time.Now().Add(3 * time.Second)
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
		a.toastExpiry = time.Now().Add(3 * time.Second)
		return
	}

	// Start recording
	size := a.cam.SensorSize()
	name := fmt.Sprintf("thermal_%s.tha", time.Now().Format("20060102_150405"))
	rec, err := recording.NewRecorder(name, size.X, size.Y)
	if err != nil {
		log.Printf("start recording: %v", err)
		a.toastMsg = fmt.Sprintf("Recording failed: %v", err)
		a.toastExpiry = time.Now().Add(3 * time.Second)
		return
	}
	a.recorder = rec
	a.toastMsg = fmt.Sprintf("Recording: %s", name)
	a.toastExpiry = time.Now().Add(3 * time.Second)
	log.Printf("recording started: %s", name)
}

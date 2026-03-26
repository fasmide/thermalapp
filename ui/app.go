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
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"thermalapp/camera"
	"thermalapp/colorize"
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
	smInited bool

	// Rotation: 0=0°, 1=90°, 2=180°, 3=270°
	rotation int

	// Toggles
	showColorbar bool
	showHelp     bool
	showLabels   bool

	// Toast notification
	toastMsg    string
	toastExpiry time.Time

	// Tag for input events
	tag bool
}

func NewApp(cam camera.Camera) *App {
	size := cam.SensorSize()
	var w app.Window
	w.Option(
		app.Title("P3 Thermal"),
		app.Size(unit.Dp(float32(size.X*3)), unit.Dp(float32(size.Y*3+80))),
	)
	return &App{
		Window: &w,
		theme:  material.NewTheme(),
		cam:    cam,
		params: colorize.Params{
			Mode:    colorize.AGCPercentile,
			Palette: colorize.PaletteInferno,
		},
		spots: []*Spot{
			NewSpot(0, SpotMin, color.NRGBA{R: 60, G: 120, B: 255, A: 230}),
			NewSpot(1, SpotMax, color.NRGBA{R: 255, G: 60, B: 60, A: 230}),
			NewSpot(2, SpotCursor, color.NRGBA{R: 180, G: 180, B: 180, A: 200}),
		},
		graphs:       make(map[int]*GraphWindow),
		showColorbar: true,
		showLabels:   true,
	}
}

func (a *App) UpdateFrame(frame *camera.Frame) {
	a.mu.Lock()
	p := a.params
	rot := a.rotation
	a.mu.Unlock()

	result := colorize.Colorize(frame, p).Rotate(rot)

	a.mu.Lock()
	a.result = result
	a.mu.Unlock()

	imgW := result.RGBA.Bounds().Dx()

	// Update spot positions and record temperatures
	const alpha = 0.15

	// Spot 0: min
	minSpot := a.spots[0]
	newMinX, newMinY := float32(result.MinX), float32(result.MinY)
	if !a.smInited {
		minSpot.X, minSpot.Y = newMinX, newMinY
	} else {
		minSpot.X += alpha * (newMinX - minSpot.X)
		minSpot.Y += alpha * (newMinY - minSpot.Y)
	}
	minSpot.Active = true
	minIdx := int(minSpot.Y)*imgW + int(minSpot.X)
	if minIdx >= 0 && minIdx < len(result.Celsius) {
		minSpot.Record(result.Celsius[minIdx])
	}

	// Spot 1: max
	maxSpot := a.spots[1]
	newMaxX, newMaxY := float32(result.MaxX), float32(result.MaxY)
	if !a.smInited {
		maxSpot.X, maxSpot.Y = newMaxX, newMaxY
	} else {
		maxSpot.X += alpha * (newMaxX - maxSpot.X)
		maxSpot.Y += alpha * (newMaxY - maxSpot.Y)
	}
	maxSpot.Active = true
	maxIdx := int(maxSpot.Y)*imgW + int(maxSpot.X)
	if maxIdx >= 0 && maxIdx < len(result.Celsius) {
		maxSpot.Record(result.Celsius[maxIdx])
	}

	a.smInited = true

	// Spot 2: cursor — recorded in handlePointer, just read temp here if active
	cursorSpot := a.spots[2]
	if cursorSpot.Active {
		cIdx := int(cursorSpot.Y)*imgW + int(cursorSpot.X)
		if cIdx >= 0 && cIdx < len(result.Celsius) {
			cursorSpot.Record(result.Celsius[cIdx])
		}
	}

	// User spots (3+)
	a.mu.Lock()
	userSpots := a.spots[3:]
	a.mu.Unlock()
	for _, sp := range userSpots {
		idx := int(sp.Y)*imgW + int(sp.X)
		if idx >= 0 && idx < len(result.Celsius) {
			sp.Record(result.Celsius[idx])
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
		key.Filter{Name: "R"},
		key.Filter{Name: "T"},
		key.Filter{Name: "X"},
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
			a.Window.Perform(system.ActionClose)

		case "C":
			a.mu.Lock()
			a.params.Palette = a.params.Palette.Next()
			a.mu.Unlock()

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

		case "T":
			a.showLabels = !a.showLabels

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
			a.mu.Unlock()

		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(ke.Name[0] - '0')
			a.toggleGraph(idx)

		case key.NameSpace:
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
				cursorSpot.X = float32(imgX)
				cursorSpot.Y = float32(imgY)
				cursorSpot.Active = true
			} else {
				cursorSpot.Active = false
			}
		case pointer.Press:
			// Add or remove a user measurement point
			imgX := int((pe.Position.X - float32(a.imgOffsetX)) / a.imgScale)
			imgY := int((pe.Position.Y - float32(a.imgOffsetY)) / a.imgScale)

			a.mu.Lock()
			r := a.result
			a.mu.Unlock()

			if r != nil && imgX >= 0 && imgY >= 0 && imgX < r.RGBA.Bounds().Dx() && imgY < r.RGBA.Bounds().Dy() {
				// Check if clicking near an existing user point (within 5px) to remove it
				removed := false
				a.mu.Lock()
				for i := 3; i < len(a.spots); i++ {
					sp := a.spots[i]
					dx := int(sp.X) - imgX
					dy := int(sp.Y) - imgY
					if dx*dx+dy*dy < 25 {
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
					sp.X = float32(imgX)
					sp.Y = float32(imgY)
					sp.Active = true
					a.spots = append(a.spots, sp)
				}
				a.mu.Unlock()
			}

		case pointer.Leave:
			a.spots[2].Active = false
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

	// Status bar
	children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
		return a.layoutStatus(gtx, result)
	}))

	// Help overlay is drawn on top after the main layout
	dims := layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)

	if a.showHelp {
		a.layoutHelp(gtx)
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
	a.mu.Unlock()

	for _, sp := range allSpots {
		if !sp.Active {
			continue
		}
		// Skip cursor for marker drawing (it gets a label at cursor position)
		if sp.Kind == SpotCursor {
			continue
		}

		mx := offsetX + int(sp.X*scale+scale/2) - markerSize
		my := offsetY + int(sp.Y*scale+scale/2) - markerSize
		sz := markerSize * 2
		s := clip.Rect{Min: image.Pt(mx, my), Max: image.Pt(mx+sz, my+sz)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, sp.Color)
		s.Pop()
	}

	// Temperature labels
	if a.showLabels {
		imgW := result.RGBA.Bounds().Dx()
		for _, sp := range allSpots {
			if !sp.Active || sp.Kind == SpotCursor {
				continue
			}
			idx := int(sp.Y)*imgW + int(sp.X)
			if idx >= 0 && idx < len(result.Celsius) {
				lx := offsetX + int(sp.X*scale+scale/2)
				ly := offsetY + int(sp.Y*scale) - 2
				a.drawSpotLabel(gtx, lx, ly, sp.Index, result.Celsius[idx], sp.Color)
			}
		}
	}

	// Cursor temperature label (next to mouse pointer)
	cursorSpot := a.spots[2]
	if cursorSpot.Active {
		cx := int(a.cursorPos.X) + 12
		cy := int(a.cursorPos.Y) - 6
		a.drawTempLabel(gtx, cx, cy, cursorSpot.LastTemp(), cursorSpot.Color)
	}

	return layout.Dimensions{Size: gtx.Constraints.Max}
}

// drawSpotLabel draws a temperature label with the spot index prefix.
func (a *App) drawSpotLabel(gtx layout.Context, sx, sy int, index int, temp float32, col color.NRGBA) {
	txt := fmt.Sprintf("[%d] %.1f\u00b0", index, temp)

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

	// Open new graph window
	gw := NewGraphWindow(a.spots[idx])
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

func (a *App) layoutStatus(gtx layout.Context, result *colorize.Result) layout.Dimensions {
	a.mu.Lock()
	p := a.params
	a.mu.Unlock()

	gainStr := "High"
	if a.gainMode == camera.GainLow {
		gainStr = "Low"
	}

	status := fmt.Sprintf("[C] %-10s  |  [A] %-10s  |  [G] Gain: %-4s  |  [R] %d\u00b0  |  [T] Labels  |  [H] Help",
		p.Palette, agcName(p.Mode), gainStr, a.rotation*90)

	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			// Dark background
			defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
			paint.Fill(gtx.Ops, color.NRGBA{R: 30, G: 30, B: 30, A: 255})
			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(6)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(a.theme, status)
				lbl.Color = color.NRGBA{R: 220, G: 220, B: 220, A: 255}
				lbl.Alignment = text.Middle
				return lbl.Layout(gtx)
			})
		},
	)
}

func (a *App) layoutHelp(gtx layout.Context) {
	type row struct{ key, desc string }
	rows := []row{
		{"Q / Esc", "Quit"},
		{"C", "Cycle colormap"},
		{"A", "Cycle AGC mode"},
		{"G", "Toggle gain (High/Low)"},
		{"S", "Trigger shutter/NUC"},
		{"R", "Rotate 90\u00b0"},
		{"T", "Toggle temp labels"},
		{"V", "Toggle colorbar"},
		{"X", "Clear user points"},
		{"Click", "Place/remove point"},
		{"0-9", "Toggle graph for spot"},
		{"Space", "Save screenshot (PNG)"},
		{"H", "Toggle this help"},
	}

	lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}
	keyW := gtx.Dp(unit.Dp(80))

	// Semi-transparent background
	defer op.Offset(image.Pt(20, 20)).Push(gtx.Ops).Pop()

	// Title + rows
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(a.theme, "Keyboard Controls")
			lbl.Color = lightGray
			lbl.Font.Weight = font.Bold
			return layout.Inset{Bottom: unit.Dp(4)}.Layout(gtx, lbl.Layout)
		}),
	}
	for _, r := range rows {
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

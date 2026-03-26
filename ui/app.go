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

// fixedPoint is a user-placed measurement point in image pixel coordinates.
type fixedPoint struct {
	X, Y int
}

// App holds the UI and shared state.
type App struct {
	Window *app.Window
	theme  *material.Theme

	mu     sync.Mutex
	result *colorize.Result
	params colorize.Params
	cam    camera.Camera

	// Cursor state
	cursorPos   f32.Point // in image-local coords
	cursorTemp  float32
	cursorValid bool

	// Image layout info (updated each frame for cursor mapping)
	imgOffsetX, imgOffsetY int
	imgScale               float32

	// Gain state
	gainMode camera.GainMode

	// Smoothed spot marker positions (EMA)
	smMinX, smMinY float32
	smMaxX, smMaxY float32
	smInited       bool

	// Rotation: 0=0°, 1=90°, 2=180°, 3=270°
	rotation int

	// Toggles
	showColorbar bool
	showHelp     bool
	showLabels   bool

	// User-placed measurement points (image pixel coords)
	fixedPoints []fixedPoint

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
		showColorbar: true,
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
			a.fixedPoints = nil

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

			if r != nil && imgX >= 0 && imgY >= 0 && imgX < r.RGBA.Bounds().Dx() && imgY < r.RGBA.Bounds().Dy() {
				idx := imgY*r.RGBA.Bounds().Dx() + imgX
				if idx < len(r.Celsius) {
					a.cursorTemp = r.Celsius[idx]
					a.cursorValid = true
				}
			} else {
				a.cursorValid = false
			}
		case pointer.Press:
			// Add or remove a fixed measurement point
			imgX := int((pe.Position.X - float32(a.imgOffsetX)) / a.imgScale)
			imgY := int((pe.Position.Y - float32(a.imgOffsetY)) / a.imgScale)

			a.mu.Lock()
			r := a.result
			a.mu.Unlock()

			if r != nil && imgX >= 0 && imgY >= 0 && imgX < r.RGBA.Bounds().Dx() && imgY < r.RGBA.Bounds().Dy() {
				// Check if clicking near an existing point (within 5px) to remove it
				removed := false
				for i, p := range a.fixedPoints {
					dx := p.X - imgX
					dy := p.Y - imgY
					if dx*dx+dy*dy < 25 {
						a.fixedPoints = append(a.fixedPoints[:i], a.fixedPoints[i+1:]...)
						removed = true
						break
					}
				}
				if !removed {
					a.fixedPoints = append(a.fixedPoints, fixedPoint{X: imgX, Y: imgY})
				}
			}

		case pointer.Leave:
			a.cursorValid = false
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

	// Smooth spot marker positions with EMA
	const alpha = 0.15 // lower = smoother, 0.1-0.2 works well at 25fps
	newMinX, newMinY := float32(result.MinX), float32(result.MinY)
	newMaxX, newMaxY := float32(result.MaxX), float32(result.MaxY)
	if !a.smInited {
		a.smMinX, a.smMinY = newMinX, newMinY
		a.smMaxX, a.smMaxY = newMaxX, newMaxY
		a.smInited = true
	} else {
		a.smMinX += alpha * (newMinX - a.smMinX)
		a.smMinY += alpha * (newMinY - a.smMinY)
		a.smMaxX += alpha * (newMaxX - a.smMaxX)
		a.smMaxY += alpha * (newMaxY - a.smMaxY)
	}

	// Draw spot markers (min=blue, max=red)
	markerSize := 4
	// Min spot (blue)
	{
		mx := offsetX + int(a.smMinX*scale+scale/2) - markerSize
		my := offsetY + int(a.smMinY*scale+scale/2) - markerSize
		sz := markerSize * 2
		s := clip.Rect{Min: image.Pt(mx, my), Max: image.Pt(mx+sz, my+sz)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 60, G: 120, B: 255, A: 230})
		s.Pop()
	}
	// Max spot (red)
	{
		mx := offsetX + int(a.smMaxX*scale+scale/2) - markerSize
		my := offsetY + int(a.smMaxY*scale+scale/2) - markerSize
		sz := markerSize * 2
		s := clip.Rect{Min: image.Pt(mx, my), Max: image.Pt(mx+sz, my+sz)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 255, G: 60, B: 60, A: 230})
		s.Pop()
	}

	// Draw user-placed fixed points (green)
	for _, p := range a.fixedPoints {
		px := offsetX + int(float32(p.X)*scale+scale/2) - markerSize
		py := offsetY + int(float32(p.Y)*scale+scale/2) - markerSize
		sz := markerSize * 2
		s := clip.Rect{Min: image.Pt(px, py), Max: image.Pt(px+sz, py+sz)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 60, G: 220, B: 60, A: 230})
		s.Pop()
	}

	// Temperature labels on markers
	if a.showLabels {
		imgW := result.RGBA.Bounds().Dx()

		// Min label
		minIdx := int(a.smMinY)*imgW + int(a.smMinX)
		if minIdx >= 0 && minIdx < len(result.Celsius) {
			a.drawTempLabel(gtx, offsetX+int(a.smMinX*scale+scale/2), offsetY+int(a.smMinY*scale)-2,
				result.Celsius[minIdx], color.NRGBA{R: 60, G: 120, B: 255, A: 230})
		}

		// Max label
		maxIdx := int(a.smMaxY)*imgW + int(a.smMaxX)
		if maxIdx >= 0 && maxIdx < len(result.Celsius) {
			a.drawTempLabel(gtx, offsetX+int(a.smMaxX*scale+scale/2), offsetY+int(a.smMaxY*scale)-2,
				result.Celsius[maxIdx], color.NRGBA{R: 255, G: 60, B: 60, A: 230})
		}

		// Fixed point labels
		for _, p := range a.fixedPoints {
			idx := p.Y*imgW + p.X
			if idx >= 0 && idx < len(result.Celsius) {
				a.drawTempLabel(gtx, offsetX+int(float32(p.X)*scale+scale/2), offsetY+int(float32(p.Y)*scale)-2,
					result.Celsius[idx], color.NRGBA{R: 60, G: 220, B: 60, A: 230})
			}
		}
	} else {
		// Even without labels, show temps on fixed points
		imgW := result.RGBA.Bounds().Dx()
		for _, p := range a.fixedPoints {
			idx := p.Y*imgW + p.X
			if idx >= 0 && idx < len(result.Celsius) {
				a.drawTempLabel(gtx, offsetX+int(float32(p.X)*scale+scale/2), offsetY+int(float32(p.Y)*scale)-2,
					result.Celsius[idx], color.NRGBA{R: 60, G: 220, B: 60, A: 230})
			}
		}
	}

	return layout.Dimensions{Size: gtx.Constraints.Max}
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

	cursor := ""
	if a.cursorValid {
		cursor = fmt.Sprintf("  Cursor: %.1f\u00b0C", a.cursorTemp)
	}

	gainStr := "High"
	if a.gainMode == camera.GainLow {
		gainStr = "Low"
	}

	status := fmt.Sprintf("[C] %-10s  |  [A] %-10s  |  [G] Gain: %-4s  |  [R] %d\u00b0  |  [T] Labels%s  |  [H] Help",
		p.Palette, agcName(p.Mode), gainStr, a.rotation*90, cursor)

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
		{"X", "Clear fixed points"},
		{"Click", "Place/remove point"},
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

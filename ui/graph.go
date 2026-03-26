package ui

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"sync"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// GraphWindow manages a separate window displaying a temperature graph for a Spot.
type GraphWindow struct {
	spot   *Spot
	window *app.Window
	theme  *material.Theme
	closed bool
	mu     sync.Mutex
}

// NewGraphWindow creates and opens a new graph window for the given spot.
// NewGraphWindow creates and opens a new graph window for the given spot.
// Each graph window gets its own theme to avoid concurrent text shaper access.
func NewGraphWindow(spot *Spot) *GraphWindow {
	var w app.Window
	w.Option(
		app.Title(fmt.Sprintf("Spot %d — Temperature Graph", spot.Index)),
		app.Size(unit.Dp(600), unit.Dp(250)),
	)
	gw := &GraphWindow{
		spot:   spot,
		window: &w,
		theme:  material.NewTheme(),
	}
	go gw.run()
	return gw
}

// IsClosed returns true if the graph window has been closed.
func (gw *GraphWindow) IsClosed() bool {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return gw.closed
}

// Invalidate triggers a redraw of the graph window.
func (gw *GraphWindow) Invalidate() {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if !gw.closed {
		gw.window.Invalidate()
	}
}

func (gw *GraphWindow) run() {
	var ops op.Ops
	for {
		switch e := gw.window.Event().(type) {
		case app.DestroyEvent:
			gw.mu.Lock()
			gw.closed = true
			gw.mu.Unlock()
			return
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			paint.Fill(gtx.Ops, color.NRGBA{R: 25, G: 25, B: 25, A: 255})
			gw.layoutGraph(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (gw *GraphWindow) layoutGraph(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// Title bar
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			spot := gw.spot
			kindStr := ""
			switch spot.Kind {
			case SpotMin:
				kindStr = "Min"
			case SpotMax:
				kindStr = "Max"
			case SpotCursor:
				kindStr = "Cursor"
			case SpotUser:
				kindStr = fmt.Sprintf("Point %d", spot.Index)
			}
			title := fmt.Sprintf("Spot %d (%s) — %.1f°C — %d samples",
				spot.Index, kindStr, spot.LastTemp(), spot.Count())

			return layout.UniformInset(unit.Dp(6)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(gw.theme, title)
				lbl.Color = color.NRGBA{R: 220, G: 220, B: 220, A: 255}
				lbl.Alignment = text.Middle
				return lbl.Layout(gtx)
			})
		}),
		// Graph area
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return gw.drawGraph(gtx)
		}),
	)
}

func (gw *GraphWindow) drawGraph(gtx layout.Context) layout.Dimensions {
	w := gtx.Constraints.Max.X
	h := gtx.Constraints.Max.Y
	if w < 10 || h < 10 {
		return layout.Dimensions{Size: image.Pt(w, h)}
	}

	margin := gtx.Dp(unit.Dp(40))
	graphX := margin
	graphW := w - margin*2
	graphY := gtx.Dp(unit.Dp(4))
	graphH := h - graphY - gtx.Dp(unit.Dp(20))

	if graphW < 10 || graphH < 10 {
		return layout.Dimensions{Size: image.Pt(w, h)}
	}

	// Get samples to display — up to graphW pixels worth
	samples := gw.spot.History(graphW)
	if len(samples) < 2 {
		s := op.Offset(image.Pt(w/2-30, h/2)).Push(gtx.Ops)
		lbl := material.Body2(gw.theme, "Waiting for data...")
		lbl.Color = color.NRGBA{R: 150, G: 150, B: 150, A: 255}
		lbl.Layout(gtx)
		s.Pop()
		return layout.Dimensions{Size: image.Pt(w, h)}
	}

	// Find min/max for Y axis
	minT, maxT := samples[0].Temp, samples[0].Temp
	for _, s := range samples {
		if s.Temp < minT {
			minT = s.Temp
		}
		if s.Temp > maxT {
			maxT = s.Temp
		}
	}

	// Add some padding to the range
	rangeT := maxT - minT
	if rangeT < 0.5 {
		rangeT = 0.5
		mid := (minT + maxT) / 2
		minT = mid - rangeT/2
		maxT = mid + rangeT/2
	} else {
		pad := rangeT * 0.05
		minT -= pad
		maxT += pad
		rangeT = maxT - minT
	}

	// Draw grid lines and Y-axis labels
	nLines := 5
	for i := 0; i <= nLines; i++ {
		frac := float32(i) / float32(nLines)
		y := graphY + int(frac*float32(graphH))
		temp := maxT - frac*rangeT

		// Grid line
		s := clip.Rect{Min: image.Pt(graphX, y), Max: image.Pt(graphX+graphW, y+1)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 60, G: 60, B: 60, A: 255})
		s.Pop()

		// Label
		ls := op.Offset(image.Pt(2, y-6)).Push(gtx.Ops)
		lbl := material.Caption(gw.theme, fmt.Sprintf("%.1f", temp))
		lbl.Color = color.NRGBA{R: 150, G: 150, B: 150, A: 255}
		lbl.Layout(gtx)
		ls.Pop()
	}

	// Draw the temperature trace
	spotColor := gw.spot.Color
	n := len(samples)
	xStep := float32(graphW) / float32(n-1)

	for i := 1; i < n; i++ {
		// Map two consecutive samples to pixel coordinates
		x0 := float32(graphX) + float32(i-1)*xStep
		x1 := float32(graphX) + float32(i)*xStep
		y0 := float32(graphY) + (1-(samples[i-1].Temp-minT)/rangeT)*float32(graphH)
		y1 := float32(graphY) + (1-(samples[i].Temp-minT)/rangeT)*float32(graphH)

		// Draw a thin rect between the two points (approximation of a line)
		drawLine(gtx, x0, y0, x1, y1, spotColor)
	}

	// Current value label at right edge
	last := samples[n-1]
	ly := float32(graphY) + (1-(last.Temp-minT)/rangeT)*float32(graphH)
	ls := op.Offset(image.Pt(graphX+graphW+4, int(ly)-6)).Push(gtx.Ops)
	lbl := material.Caption(gw.theme, fmt.Sprintf("%.1f°", last.Temp))
	lbl.Color = spotColor
	lbl.Layout(gtx)
	ls.Pop()

	return layout.Dimensions{Size: image.Pt(w, h)}
}

// drawLine draws a 2px wide line between two points.
func drawLine(gtx layout.Context, x0, y0, x1, y1 float32, col color.NRGBA) {
	dx := x1 - x0
	dy := y1 - y0
	length := float32(math.Sqrt(float64(dx*dx + dy*dy)))
	if length < 0.5 {
		return
	}

	// Draw as a series of small rectangles along the line
	steps := int(length) + 1
	for s := 0; s < steps; s++ {
		t := float32(s) / float32(steps)
		px := int(x0 + dx*t)
		py := int(y0 + dy*t)
		r := clip.Rect{Min: image.Pt(px, py), Max: image.Pt(px+2, py+2)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, col)
		r.Pop()
	}
}

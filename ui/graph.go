package ui

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"thermalapp/colorize"
)

const (
	graphWindowWidthDp  = 600    // initial width of a graph window in dp
	graphWindowHeightDp = 250    // initial height of a graph window in dp
	graphMarginDp       = 40     // left/right margin for graph axes in dp
	graphBottomDp       = 20     // bottom margin for graph in dp
	graphWaitMsgXOff    = 30     // x offset for "waiting" message from center
	minSamplesForGraph  = 2      // minimum samples required to draw the graph
	minTempRangeC       = 3.0    // minimum temperature range for Y axis (°C)
	graphTitleInsetDp   = 4      // uniform inset inside the graph title bar
	graphTopMarginDp    = 4      // top margin before the first graph Y tick in dp
	graphLabelXOff      = 4      // x pixel offset from graph right edge to current-value label
	graphLabelYOff      = 6      // y pixel half-height correction for current-value label
	graphPadFrac        = 0.05   // fractional padding added to Y axis range on each side
	msPerSecond         = 1000.0 // milliseconds per second (for render-time conversion)
	graphCenterDiv      = 2      // divisor for centering labels horizontally/vertically
	graphAxisLabelXOff  = 2      // x offset for Y-axis temperature labels from left edge
	graphAxisLabelYAdj  = 6      // y pixel upward adjustment for Y-axis temperature labels
	drawLineMinLength   = 0.5    // minimum line length (px) before drawLine is a no-op
	drawLinePixelSize   = 2      // pixel width/height of each segment in drawLine

	// LTTB downsampling algorithm constants.
	lttbBoundaryPoints = 2 // number of fixed boundary points (first + last) in LTTB
	lttbBucketOffset   = 1 // bucket index offset used in LTTB bucket averaging

	// rightTitleFmt is the format string for the right side of the graph title bar.
	rightTitleFmt = "|  Now: %.1f°C  |  Min: %.1f  Max: %.1f  Mean: %.1f  s: %.2f" +
		"  |  %d / %s  |  %.0fms  E2E %.0fms"
)

// GraphWindow manages a separate window displaying a temperature graph for a Spot.
type GraphWindow struct {
	spot        *Spot
	pixSrc      PixelQuerier
	window      *app.Window
	theme       *material.Theme
	closed      bool
	mu          sync.Mutex
	epsDropdown *EmissivityDropdown
	epsClick    widget.Clickable
	renderMs    float64 // last frame render time in milliseconds
}

// NewGraphWindow creates and opens a new graph window for the given spot.
// Each graph window gets its own theme to avoid concurrent text shaper access.
func NewGraphWindow(spot *Spot, pixSrc PixelQuerier) *GraphWindow {
	var w app.Window
	w.Option(
		app.Title(fmt.Sprintf("Spot %d — Temperature Graph", spot.Index)),
		app.Size(unit.Dp(graphWindowWidthDp), unit.Dp(graphWindowHeightDp)),
	)
	gw := &GraphWindow{
		spot:        spot,
		pixSrc:      pixSrc,
		window:      &w,
		theme:       material.NewTheme(),
		epsDropdown: NewEmissivityDropdown(),
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
	start := time.Now()
	defer func() { gw.renderMs = float64(time.Since(start).Microseconds()) / msPerSecond }()

	// Fetch graph data: buffer for cursor/user, spot ring buffer for min/max
	var allSamples []Sample
	switch gw.spot.Kind {
	case SpotMin, SpotMax:
		allSamples = gw.spot.History(0)
	default:
		x, y := gw.spot.GetPosition()
		allSamples = gw.pixSrc.QueryPixel(int(x), int(y), 0)
	}
	st := ComputeStats(allSamples)

	dims := layout.Flex{Axis: layout.Vertical}.Layout(gtx,
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

			dur := st.Duration.Truncate(time.Second)

			epsLabel := " e: global "
			_, spotEpsIdx := spot.GetEmissivity()
			if spotEpsIdx >= 0 && spotEpsIdx < len(colorize.EmissivityPresets) {
				p := colorize.EmissivityPresets[spotEpsIdx]
				epsLabel = fmt.Sprintf(" e: %.2f %s ", p.Emissivity, p.Name)
			}

			leftTitle := fmt.Sprintf("Spot %d (%s)  |", spot.Index, kindStr)
			latencyMs := float64(time.Since(spot.LastMoveTime()).Microseconds()) / msPerSecond
			rightTitle := fmt.Sprintf(rightTitleFmt,
				st.Current, st.Min, st.Max, st.Mean, st.StdDev, st.Count, dur, gw.renderMs, latencyMs)

			lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}

			currentIdx := spotEpsIdx
			if gw.epsClick.Clicked(gtx) {
				gw.epsDropdown.Toggle(currentIdx)
			}

			return layout.UniformInset(unit.Dp(graphTitleInsetDp)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(gw.theme, leftTitle)
						lbl.Color = lightGray

						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return dropdownButton(gtx, gw.theme, &gw.epsClick, gw.epsDropdown.IsOpen(), epsLabel)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(gw.theme, rightTitle)
						lbl.Color = lightGray

						return lbl.Layout(gtx)
					}),
				)
			})
		}),
		// Graph area
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return gw.drawGraph(gtx, allSamples)
		}),
	)

	// Emissivity dropdown overlay
	if gw.epsDropdown.IsOpen() {
		_, currentIdx := gw.spot.GetEmissivity()
		// For per-spot, -1 means "global" — map to preset list idx
		displayIdx := currentIdx
		if sel := gw.epsDropdown.Layout(gtx, gw.theme, displayIdx); sel >= 0 {
			preset := colorize.EmissivityPresets[sel]
			gw.spot.SetEmissivity(preset.Emissivity, sel)
		}
	}

	return dims
}

func (gw *GraphWindow) drawGraph(gtx layout.Context, allSamples []Sample) layout.Dimensions {
	w := gtx.Constraints.Max.X
	h := gtx.Constraints.Max.Y
	if w < 10 || h < 10 {
		return layout.Dimensions{Size: image.Pt(w, h)}
	}

	margin := gtx.Dp(unit.Dp(graphMarginDp))
	graphX := margin
	graphW := w - margin*2
	graphY := gtx.Dp(unit.Dp(graphTopMarginDp))
	graphH := h - graphY - gtx.Dp(unit.Dp(graphBottomDp))

	if graphW < 10 || graphH < 10 {
		return layout.Dimensions{Size: image.Pt(w, h)}
	}

	// Get samples to display — downsample to graphW points if needed
	samples := allSamples
	if len(samples) > graphW && graphW > 0 {
		samples = downsample(allSamples, graphW)
	}
	if len(samples) < minSamplesForGraph {
		s := op.Offset(image.Pt(w/graphCenterDiv-graphWaitMsgXOff, h/graphCenterDiv)).Push(gtx.Ops)
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

	// Add some padding to the range (minimum minTempRangeC so noise doesn't dominate)
	rangeT := maxT - minT
	if rangeT < minTempRangeC {
		mid := (minT + maxT) / 2
		minT = mid - minTempRangeC/2
		maxT = mid + minTempRangeC/2
		rangeT = minTempRangeC
	} else {
		pad := rangeT * graphPadFrac
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
		ls := op.Offset(image.Pt(graphAxisLabelXOff, y-graphAxisLabelYAdj)).Push(gtx.Ops)
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
	ls := op.Offset(image.Pt(graphX+graphW+graphLabelXOff, int(ly)-graphLabelYOff)).Push(gtx.Ops)
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
	if length < drawLineMinLength {
		return
	}

	// Draw as a series of small rectangles along the line
	steps := int(length) + 1
	for s := range steps {
		t := float32(s) / float32(steps)
		px := int(x0 + dx*t)
		py := int(y0 + dy*t)
		r := clip.Rect{Min: image.Pt(px, py), Max: image.Pt(px+drawLinePixelSize, py+drawLinePixelSize)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, col)
		r.Pop()
	}
}

// downsample reduces samples to n points using LTTB (Largest-Triangle-Three-Buckets)
// which preserves the visual shape of the data much better than simple decimation.
func downsample(data []Sample, n int) []Sample {
	if n >= len(data) || n < 3 {
		return data
	}

	out := make([]Sample, 0, n)
	out = append(out, data[0]) // Always keep first

	bucketSize := float64(len(data)-lttbBoundaryPoints) / float64(n-lttbBoundaryPoints)

	a := 0 // Index of the previous selected point

	for i := 1; i < n-1; i++ {
		// Calculate the average of the next bucket (for the triangle)
		avgStart := int(float64(i+lttbBucketOffset)*bucketSize) + lttbBucketOffset
		avgEnd := int(float64(i+lttbBoundaryPoints)*bucketSize) + lttbBucketOffset
		if avgEnd > len(data) {
			avgEnd = len(data)
		}
		if avgStart >= avgEnd {
			avgStart = avgEnd - 1
		}
		var avgX, avgY float64
		for j := avgStart; j < avgEnd; j++ {
			avgX += float64(j)
			avgY += float64(data[j].Temp)
		}
		avgCount := float64(avgEnd - avgStart)
		avgX /= avgCount
		avgY /= avgCount

		// Current bucket range
		bStart := int(float64(i)*bucketSize) + 1
		bEnd := int(float64(i+1)*bucketSize) + 1
		if bEnd > len(data) {
			bEnd = len(data)
		}

		// Find the point in this bucket that forms the largest triangle
		// with the previous selected point and the next-bucket average
		maxArea := -1.0
		bestIdx := bStart
		ax := float64(a)
		ay := float64(data[a].Temp)

		for j := bStart; j < bEnd; j++ {
			// Triangle area (doubled, no abs needed for comparison)
			area := math.Abs((ax-avgX)*(float64(data[j].Temp)-ay) -
				(ax-float64(j))*(avgY-ay))
			if area > maxArea {
				maxArea = area
				bestIdx = j
			}
		}

		out = append(out, data[bestIdx])
		a = bestIdx
	}

	out = append(out, data[len(data)-1]) // Always keep last

	return out
}

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
	graphMarginMult     = 2      // multiplier for both left+right margins when computing graph width

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
	var win app.Window
	win.Option(
		app.Title(fmt.Sprintf("Spot %d — Temperature Graph", spot.Index)),
		app.Size(unit.Dp(graphWindowWidthDp), unit.Dp(graphWindowHeightDp)),
	)
	graphWin := &GraphWindow{
		spot:        spot,
		pixSrc:      pixSrc,
		window:      &win,
		theme:       material.NewTheme(),
		epsDropdown: NewEmissivityDropdown(),
	}
	go graphWin.run()

	return graphWin
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
		switch winEv := gw.window.Event().(type) {
		case app.DestroyEvent:
			gw.mu.Lock()
			gw.closed = true
			gw.mu.Unlock()

			return
		case app.FrameEvent:
			gtx := app.NewContext(&ops, winEv)
			paint.Fill(gtx.Ops, color.NRGBA{R: 25, G: 25, B: 25, A: 255})
			gw.layoutGraph(gtx)
			winEv.Frame(gtx.Ops)
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
	stats := ComputeStats(allSamples)

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

			dur := stats.Duration.Truncate(time.Second)

			epsLabel := " e: global "
			_, spotEpsIdx := spot.GetEmissivity()
			if spotEpsIdx >= 0 && spotEpsIdx < len(colorize.EmissivityPresets) {
				p := colorize.EmissivityPresets[spotEpsIdx]
				epsLabel = fmt.Sprintf(" e: %.2f %s ", p.Emissivity, p.Name)
			}

			leftTitle := fmt.Sprintf("Spot %d (%s)  |", spot.Index, kindStr)
			latencyMs := float64(time.Since(spot.LastMoveTime()).Microseconds()) / msPerSecond
			rightTitle := fmt.Sprintf(rightTitleFmt,
				stats.Current, stats.Min, stats.Max, stats.Mean, stats.StdDev, stats.Count, dur, gw.renderMs, latencyMs)

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
	graphAreaW := gtx.Constraints.Max.X
	graphAreaH := gtx.Constraints.Max.Y
	if graphAreaW < 10 || graphAreaH < 10 {
		return layout.Dimensions{Size: image.Pt(graphAreaW, graphAreaH)}
	}

	margin := gtx.Dp(unit.Dp(graphMarginDp))
	graphX := margin
	graphW := graphAreaW - margin*graphMarginMult
	graphY := gtx.Dp(unit.Dp(graphTopMarginDp))
	graphH := graphAreaH - graphY - gtx.Dp(unit.Dp(graphBottomDp))

	if graphW < 10 || graphH < 10 {
		return layout.Dimensions{Size: image.Pt(graphAreaW, graphAreaH)}
	}

	// Get samples to display — downsample to graphW points if needed
	samples := allSamples
	if len(samples) > graphW && graphW > 0 {
		samples = downsample(allSamples, graphW)
	}
	if len(samples) < minSamplesForGraph {
		waitOp := op.Offset(image.Pt(graphAreaW/graphCenterDiv-graphWaitMsgXOff, graphAreaH/graphCenterDiv)).Push(gtx.Ops)
		lbl := material.Body2(gw.theme, "Waiting for data...")
		lbl.Color = color.NRGBA{R: 150, G: 150, B: 150, A: 255}
		lbl.Layout(gtx)
		waitOp.Pop()

		return layout.Dimensions{Size: image.Pt(graphAreaW, graphAreaH)}
	}

	// Find min/max for Y axis
	minT, maxT := samples[0].Temp, samples[0].Temp
	for _, samp := range samples {
		if samp.Temp < minT {
			minT = samp.Temp
		}
		if samp.Temp > maxT {
			maxT = samp.Temp
		}
	}

	// Add some padding to the range (minimum minTempRangeC so noise doesn't dominate)
	rangeT := maxT - minT
	if rangeT < minTempRangeC {
		mid := (minT + maxT) / graphCenterDiv
		minT = mid - minTempRangeC/graphCenterDiv
		maxT = mid + minTempRangeC/graphCenterDiv
		rangeT = minTempRangeC
	} else {
		pad := rangeT * graphPadFrac
		minT -= pad
		maxT += pad
		rangeT = maxT - minT
	}

	// Draw grid lines and Y-axis labels
	nLines := 5
	for gridLine := 0; gridLine <= nLines; gridLine++ {
		frac := float32(gridLine) / float32(nLines)
		gridY := graphY + int(frac*float32(graphH))
		temp := maxT - frac*rangeT

		// Grid line
		gridOp := clip.Rect{Min: image.Pt(graphX, gridY), Max: image.Pt(graphX+graphW, gridY+1)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{R: 60, G: 60, B: 60, A: 255})
		gridOp.Pop()

		// Label
		labelOp := op.Offset(image.Pt(graphAxisLabelXOff, gridY-graphAxisLabelYAdj)).Push(gtx.Ops)
		lbl := material.Caption(gw.theme, fmt.Sprintf("%.1f", temp))
		lbl.Color = color.NRGBA{R: 150, G: 150, B: 150, A: 255}
		lbl.Layout(gtx)
		labelOp.Pop()
	}

	// Draw the temperature trace
	spotColor := gw.spot.Color
	sampleCount := len(samples)
	xStep := float32(graphW) / float32(sampleCount-1)

	for sampleIdx := 1; sampleIdx < sampleCount; sampleIdx++ {
		// Map two consecutive samples to pixel coordinates
		fromX := float32(graphX) + float32(sampleIdx-1)*xStep
		toX := float32(graphX) + float32(sampleIdx)*xStep
		fromY := float32(graphY) + (1-(samples[sampleIdx-1].Temp-minT)/rangeT)*float32(graphH)
		toY := float32(graphY) + (1-(samples[sampleIdx].Temp-minT)/rangeT)*float32(graphH)

		// Draw a thin rect between the two points (approximation of a line)
		drawLine(gtx, fromX, fromY, toX, toY, spotColor)
	}

	// Current value label at right edge
	last := samples[sampleCount-1]
	ly := float32(graphY) + (1-(last.Temp-minT)/rangeT)*float32(graphH)
	ls := op.Offset(image.Pt(graphX+graphW+graphLabelXOff, int(ly)-graphLabelYOff)).Push(gtx.Ops)
	lbl := material.Caption(gw.theme, fmt.Sprintf("%.1f°", last.Temp))
	lbl.Color = spotColor
	lbl.Layout(gtx)
	ls.Pop()

	return layout.Dimensions{Size: image.Pt(graphAreaW, graphAreaH)}
}

// drawLine draws a 2px wide line between two points.
func drawLine(gtx layout.Context, fromX, fromY, toX, toY float32, col color.NRGBA) {
	deltaX := toX - fromX
	deltaY := toY - fromY
	length := float32(math.Sqrt(float64(deltaX*deltaX + deltaY*deltaY)))
	if length < drawLineMinLength {
		return
	}

	// Draw as a series of small rectangles along the line
	steps := int(length) + 1
	for stepIdx := range steps {
		frac := float32(stepIdx) / float32(steps)
		px := int(fromX + deltaX*frac)
		py := int(fromY + deltaY*frac)
		pixOp := clip.Rect{Min: image.Pt(px, py), Max: image.Pt(px+drawLinePixelSize, py+drawLinePixelSize)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, col)
		pixOp.Pop()
	}
}

// downsample reduces samples to n points using LTTB (Largest-Triangle-Three-Buckets)
// which preserves the visual shape of the data much better than simple decimation.
func downsample(data []Sample, targetCount int) []Sample {
	if targetCount >= len(data) || targetCount < 3 {
		return data
	}

	out := make([]Sample, 0, targetCount)
	out = append(out, data[0]) // Always keep first

	bucketSize := float64(len(data)-lttbBoundaryPoints) / float64(targetCount-lttbBoundaryPoints)

	prevIdx := 0 // Index of the previous selected point

	for bucketIdx := 1; bucketIdx < targetCount-1; bucketIdx++ {
		// Calculate the average of the next bucket (for the triangle)
		avgStart := int(float64(bucketIdx+lttbBucketOffset)*bucketSize) + lttbBucketOffset
		avgEnd := int(float64(bucketIdx+lttbBoundaryPoints)*bucketSize) + lttbBucketOffset
		if avgEnd > len(data) {
			avgEnd = len(data)
		}
		if avgStart >= avgEnd {
			avgStart = avgEnd - 1
		}
		var avgX, avgY float64
		for jj := avgStart; jj < avgEnd; jj++ {
			avgX += float64(jj)
			avgY += float64(data[jj].Temp)
		}
		avgCount := float64(avgEnd - avgStart)
		avgX /= avgCount
		avgY /= avgCount

		// Current bucket range
		bStart := int(float64(bucketIdx)*bucketSize) + 1
		bEnd := int(float64(bucketIdx+1)*bucketSize) + 1
		if bEnd > len(data) {
			bEnd = len(data)
		}

		// Find the point in this bucket that forms the largest triangle
		// with the previous selected point and the next-bucket average
		maxArea := -1.0
		bestIdx := bStart
		prevX := float64(prevIdx)
		prevY := float64(data[prevIdx].Temp)

		for innerIdx := bStart; innerIdx < bEnd; innerIdx++ {
			// Triangle area (doubled, no abs needed for comparison)
			area := math.Abs((prevX-avgX)*(float64(data[innerIdx].Temp)-prevY) -
				(prevX-float64(innerIdx))*(avgY-prevY))
			if area > maxArea {
				maxArea = area
				bestIdx = innerIdx
			}
		}

		out = append(out, data[bestIdx])
		prevIdx = bestIdx
	}

	out = append(out, data[len(data)-1]) // Always keep last

	return out
}

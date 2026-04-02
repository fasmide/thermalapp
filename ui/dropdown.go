package ui

import (
	"fmt"
	"image"
	"image/color"
	"time"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"thermalapp/colorize"
)

const (
	epsDropdownItemHeightDp = 26  // height of each emissivity dropdown item in dp
	epsDropdownWidthDp      = 280 // width of the emissivity dropdown in dp
	statusBarHeightDp       = 40  // height of the status bar in dp (used for dropdown positioning)
	dropdownHeaderTopPadDp  = 6   // top padding for dropdown category headers in dp
	dropdownItemLeftInsetDp = 16  // left inset for dropdown items in dp
	hoursPerDay             = 24  // hours per day for formatDuration
	bufPanelWidthDp         = 420 // width of the buffer settings panel in dp
	bufPanelHeightDp        = 460 // height of the buffer settings panel in dp

	// Inset constants for dropdown header row.
	dropdownHeaderInsetDp = 8 // left/right inset for category header rows

	// Inset constants for dropdown item rows.
	dropdownItemRightInsetDp = 10 // right inset for dropdown items
	dropdownItemVInsetDp     = 3  // top/bottom inset for dropdown items
	dropdownDividerInsetDp   = 4  // top/bottom inset for the divider line

	// Frame size calculation.
	frameBytesPerSample = 4  // bytes per pixel in the frame buffer (float32)
	frameOverheadBytes  = 64 // fixed overhead bytes added to each frame

	// Effective camera interval when none is configured (≈25 fps).
	defaultCameraInterval = 40 * time.Millisecond

	// Dropdown button (pill) insets.
	dropdownBtnHInsetDp = 6 // left/right inset for the dropdown pill button label
	dropdownBtnVInsetDp = 2 // top/bottom inset for the dropdown pill button label

	// Buffer panel insets.
	bufPanelHInsetDp        = 12 // left/right inset for buffer panel sections
	bufPanelItemLInsetDp    = 16 // left inset for buffer panel list items
	bufPanelItemRInsetDp    = 12 // right inset for buffer panel list items
	bufPanelItemVInsetDp    = 3  // top/bottom inset for buffer panel list items
	bufPanelDivVInsetDp     = 4  // top/bottom inset for buffer panel divider
	bufPanelHdrTopInsetDp   = 10 // top inset for buffer panel header section
	bufPanelSectionVInsetDp = 2  // top/bottom inset for buffer panel section headers
	bufPanelHeaderBotDp     = 2  // bottom inset for dropdown category header row
)

// dropdownButton renders a small clickable pill button used to open dropdowns
// in the status bar. It highlights on hover and turns blue when isOpen is true.
func dropdownButton(
	gtx layout.Context, th *material.Theme, click *widget.Clickable, isOpen bool, label string,
) layout.Dimensions {
	lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}

	return material.Clickable(gtx, click, func(gtx layout.Context) layout.Dimensions {
		return layout.Background{}.Layout(gtx,
			func(gtx layout.Context) layout.Dimensions {
				bgCol := color.NRGBA{R: 50, G: 50, B: 50, A: 255}
				if click.Hovered() {
					bgCol = color.NRGBA{R: 70, G: 70, B: 70, A: 255}
				}
				if isOpen {
					bgCol = color.NRGBA{R: 60, G: 90, B: 160, A: 255}
				}
				defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
				paint.Fill(gtx.Ops, bgCol)

				return layout.Dimensions{Size: gtx.Constraints.Min}
			},
			func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{
					Left: unit.Dp(dropdownBtnHInsetDp), Right: unit.Dp(dropdownBtnHInsetDp),
					Top: unit.Dp(dropdownBtnVInsetDp), Bottom: unit.Dp(dropdownBtnVInsetDp),
				}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th, label)
					lbl.Color = lightGray

					return lbl.Layout(gtx)
				})
			},
		)
	})
}

// dropdownRow is either a category header or a preset item.
type dropdownRow struct {
	isHeader    bool
	category    string
	presetIndex int // index into EmissivityPresets (only valid if !isHeader)
}

// EmissivityDropdown manages a scrollable popup list of emissivity presets.
type EmissivityDropdown struct {
	open     bool
	list     widget.List
	items    []widget.Clickable
	rows     []dropdownRow // flat list of headers + items
	tag      bool
	scrollTo int // row index to scroll to, -1 means none
}

// NewEmissivityDropdown creates a dropdown for emissivity presets.
func NewEmissivityDropdown() *EmissivityDropdown {
	presets := colorize.EmissivityPresets
	d := &EmissivityDropdown{
		items:    make([]widget.Clickable, len(presets)),
		scrollTo: -1,
	}
	d.list.List.Axis = layout.Vertical

	// Build flat row list with category headers
	lastCat := ""
	for i, p := range presets {
		if p.Category != lastCat {
			d.rows = append(d.rows, dropdownRow{isHeader: true, category: p.Category})
			lastCat = p.Category
		}
		d.rows = append(d.rows, dropdownRow{presetIndex: i})
	}

	return d
}

// presetToRow maps a preset index to its row index for scrolling.
func (d *EmissivityDropdown) presetToRow(presetIdx int) int {
	for i, r := range d.rows {
		if !r.isHeader && r.presetIndex == presetIdx {
			return i
		}
	}

	return 0
}

// Toggle opens or closes the dropdown. When opening, scrolls to the current selection.
func (d *EmissivityDropdown) Toggle(currentIdx int) {
	d.open = !d.open
	if d.open && currentIdx >= 0 {
		d.scrollTo = d.presetToRow(currentIdx)
	}
}

// Close closes the dropdown.
func (d *EmissivityDropdown) Close() {
	d.open = false
}

// IsOpen returns whether the dropdown is currently open.
func (d *EmissivityDropdown) IsOpen() bool {
	return d.open
}

// Layout draws the dropdown overlay and returns the selected index, or -1 if none.
func (d *EmissivityDropdown) Layout(gtx layout.Context, th *material.Theme, currentIdx int) int {
	if !d.open {
		return -1
	}

	selected := -1

	// Handle Escape to close
	for {
		ev, ok := gtx.Source.Event(key.Filter{Name: key.NameEscape})
		if !ok {
			break
		}
		if ke, ok := ev.(key.Event); ok && ke.State == key.Press {
			d.Close()

			return -1
		}
	}

	// Dismiss on click outside — we draw a transparent backdrop
	backdropTag := &d.tag
	area := clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops)
	event.Op(gtx.Ops, backdropTag)
	area.Pop()
	for {
		ev, ok := gtx.Source.Event(pointer.Filter{
			Target: backdropTag,
			Kinds:  pointer.Press,
		})
		if !ok {
			break
		}
		if pe, ok := ev.(pointer.Event); ok && pe.Kind == pointer.Press {
			d.Close()

			return -1
		}
	}

	// Scroll to current selection if requested
	if d.scrollTo >= 0 {
		d.list.List.ScrollTo(d.scrollTo)
		d.scrollTo = -1
	}

	// Dropdown dimensions
	itemH := gtx.Dp(unit.Dp(epsDropdownItemHeightDp))
	dropW := gtx.Dp(unit.Dp(epsDropdownWidthDp))
	maxVisibleItems := 16
	nRows := len(d.rows)
	visibleRows := nRows
	if visibleRows > maxVisibleItems {
		visibleRows = maxVisibleItems
	}
	dropH := itemH * visibleRows

	// Position: bottom-center of window (above status bar)
	x := (gtx.Constraints.Max.X - dropW) / 2
	y := gtx.Constraints.Max.Y - dropH - gtx.Dp(unit.Dp(statusBarHeightDp))
	if y < 0 {
		y = 0
	}

	s := op.Offset(image.Pt(x, y)).Push(gtx.Ops)

	// Background
	bgClip := clip.Rect{Max: image.Pt(dropW, dropH)}.Push(gtx.Ops)
	paint.Fill(gtx.Ops, color.NRGBA{R: 35, G: 35, B: 35, A: 245})
	bgClip.Pop()

	// Block pointer events from going through to the image below
	blockArea := clip.Rect{Max: image.Pt(dropW, dropH)}.Push(gtx.Ops)
	event.Op(gtx.Ops, &d.list)
	blockArea.Pop()

	// Scrollable list of rows (headers + items)
	gtx.Constraints = layout.Exact(image.Pt(dropW, dropH))
	listStyle := material.List(th, &d.list)
	headerColor := color.NRGBA{R: 140, G: 160, B: 200, A: 255}
	lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}
	dimGray := color.NRGBA{R: 160, G: 160, B: 160, A: 255}

	listStyle.Layout(gtx, nRows, func(gtx layout.Context, rowIdx int) layout.Dimensions {
		row := d.rows[rowIdx]

		if row.isHeader {
			// Category header — not clickable
			return layout.Inset{
				Left: unit.Dp(dropdownHeaderInsetDp), Right: unit.Dp(dropdownHeaderInsetDp),
				Top: unit.Dp(dropdownHeaderTopPadDp), Bottom: unit.Dp(bufPanelHeaderBotDp),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, row.category)
				lbl.Color = headerColor
				lbl.Font.Weight = font.Bold

				return lbl.Layout(gtx)
			})
		}

		// Preset item
		i := row.presetIndex
		preset := colorize.EmissivityPresets[i]

		if d.items[i].Clicked(gtx) {
			selected = i
			d.Close()
		}

		isSelected := i == currentIdx
		isHovered := d.items[i].Hovered()

		bgColor := color.NRGBA{A: 0}
		if isSelected {
			bgColor = color.NRGBA{R: 60, G: 90, B: 160, A: 255}
		} else if isHovered {
			bgColor = color.NRGBA{R: 55, G: 55, B: 55, A: 255}
		}

		return d.items[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					if bgColor.A > 0 {
						defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
						paint.Fill(gtx.Ops, bgColor)
					}

					return layout.Dimensions{Size: gtx.Constraints.Min}
				}),
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{
						Left: unit.Dp(dropdownItemLeftInsetDp), Right: unit.Dp(dropdownItemRightInsetDp),
						Top: unit.Dp(dropdownItemVInsetDp), Bottom: unit.Dp(dropdownItemVInsetDp),
					}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceBetween}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body2(th, preset.Name)
								lbl.Color = lightGray
								if isSelected {
									lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
								}

								return lbl.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								txt := fmt.Sprintf("%.2f", preset.Emissivity)
								lbl := material.Body2(th, txt)
								lbl.Color = dimGray
								if isSelected {
									lbl.Color = color.NRGBA{R: 200, G: 200, B: 255, A: 255}
								}
								lbl.Alignment = text.End

								return lbl.Layout(gtx)
							}),
						)
					})
				}),
			)
		})
	})

	// Border
	borderClip := clip.Stroke{
		Path:  clip.Rect{Max: image.Pt(dropW, dropH)}.Path(),
		Width: float32(gtx.Dp(unit.Dp(1))),
	}.Op().Push(gtx.Ops)
	paint.Fill(gtx.Ops, color.NRGBA{R: 80, G: 80, B: 80, A: 255})
	borderClip.Pop()

	s.Pop()

	return selected
}

// ---------------------------------------------------------------------------
// BufferPanel — combined buffer size + sample rate settings panel.
// ---------------------------------------------------------------------------

type bufSizePreset struct {
	Label string
	Bytes int64
}

var bufSizePresets = []bufSizePreset{
	{"100 MB", 100 << 20},
	{"250 MB", 250 << 20},
	{"500 MB", 500 << 20},
	{"1 GB", 1 << 30},
	{"2 GB", 2 << 30},
	{"4 GB", 4 << 30},
	{"8 GB", 8 << 30},
	{"16 GB", 16 << 30},
	{"32 GB", 32 << 30},
}

type sampleRatePreset struct {
	Label    string
	Interval time.Duration
}

var sampleRatePresets = []sampleRatePreset{
	{"Max (every frame)", 0},
	{"20 fps", 50 * time.Millisecond},
	{"10 fps", 100 * time.Millisecond},
	{"5 fps", 200 * time.Millisecond},
	{"1 fps", time.Second},
	{"1 / 5s", 5 * time.Second},
	{"1 / 10s", 10 * time.Second},
	{"1 / 30s", 30 * time.Second},
	{"1 / 1 min", time.Minute},
	{"1 / 5 min", 5 * time.Minute},
	{"1 / 10 min", 10 * time.Minute},
}

// BufferPanelResult holds what changed when the user interacts with the panel.
type BufferPanelResult struct {
	SizeChanged     bool
	NewBytes        int64
	IntervalChanged bool
	NewInterval     time.Duration
}

// BufferPanel manages the popup panel for buffer size and sample rate.
type BufferPanel struct {
	open      bool
	tag       bool
	sizeItems []widget.Clickable
	rateItems []widget.Clickable
	sizeList  widget.List
	rateList  widget.List
}

// NewBufferPanel creates a new buffer settings panel.
func NewBufferPanel() *BufferPanel {
	p := &BufferPanel{
		sizeItems: make([]widget.Clickable, len(bufSizePresets)),
		rateItems: make([]widget.Clickable, len(sampleRatePresets)),
	}
	p.sizeList.List.Axis = layout.Vertical
	p.rateList.List.Axis = layout.Vertical

	return p
}

func (p *BufferPanel) Toggle()      { p.open = !p.open }
func (p *BufferPanel) Close()       { p.open = false }
func (p *BufferPanel) IsOpen() bool { return p.open }

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}

	return x
}

func (p *BufferPanel) sizeIdx(b int64) int {
	best := 0
	for i, pr := range bufSizePresets {
		if abs64(pr.Bytes-b) < abs64(bufSizePresets[best].Bytes-b) {
			best = i
		}
	}

	return best
}

func (p *BufferPanel) rateIdx(d time.Duration) int {
	best := 0
	bestDiff := absDur(sampleRatePresets[0].Interval - d)
	for i, pr := range sampleRatePresets {
		diff := absDur(pr.Interval - d)
		if diff < bestDiff {
			bestDiff = diff
			best = i
		}
	}

	return best
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}

	return d
}

// formatDuration produces a human-friendly duration string.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f sec", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1f min", d.Minutes())
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%.1f hrs", d.Hours())
	}

	return fmt.Sprintf("%.1f days", d.Hours()/hoursPerDay)
}

// Layout draws the panel overlay. Returns a result indicating what changed.
func (p *BufferPanel) Layout(gtx layout.Context, th *material.Theme, currentBytes int64, currentInterval time.Duration, frameBytesPerPixel int, sensorPixels int) BufferPanelResult {
	var res BufferPanelResult
	if !p.open {
		return res
	}

	// Handle Escape
	for {
		ev, ok := gtx.Source.Event(key.Filter{Name: key.NameEscape})
		if !ok {
			break
		}
		if ke, ok := ev.(key.Event); ok && ke.State == key.Press {
			p.Close()

			return res
		}
	}

	// Backdrop dismiss
	backdropTag := &p.tag
	area := clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops)
	event.Op(gtx.Ops, backdropTag)
	area.Pop()
	for {
		ev, ok := gtx.Source.Event(pointer.Filter{Target: backdropTag, Kinds: pointer.Press})
		if !ok {
			break
		}
		if pe, ok := ev.(pointer.Event); ok && pe.Kind == pointer.Press {
			p.Close()

			return res
		}
	}

	// Compute estimates
	frameBytes := int64(sensorPixels)*frameBytesPerSample + frameOverheadBytes
	maxFrames := currentBytes / frameBytes
	if maxFrames < 1 {
		maxFrames = 1
	}
	effectiveInterval := currentInterval
	if effectiveInterval == 0 {
		effectiveInterval = defaultCameraInterval // ~25fps camera
	}
	coverage := time.Duration(maxFrames) * effectiveInterval

	avail := availableMemoryBytes()
	availStr := "N/A"
	if avail > 0 {
		availStr = humanSizeDropdown(avail)
	}

	// Panel dimensions
	panelW := gtx.Dp(unit.Dp(bufPanelWidthDp))
	panelH := gtx.Dp(unit.Dp(bufPanelHeightDp))
	x := (gtx.Constraints.Max.X - panelW) / 2
	y := (gtx.Constraints.Max.Y - panelH) / 2
	if y < 0 {
		y = 0
	}

	s := op.Offset(image.Pt(x, y)).Push(gtx.Ops)

	// Background
	bgClip := clip.Rect{Max: image.Pt(panelW, panelH)}.Push(gtx.Ops)
	paint.Fill(gtx.Ops, color.NRGBA{R: 30, G: 30, B: 30, A: 250})
	bgClip.Pop()

	// Block pointer events
	blockArea := clip.Rect{Max: image.Pt(panelW, panelH)}.Push(gtx.Ops)
	event.Op(gtx.Ops, &p.sizeList)
	blockArea.Pop()

	gtx.Constraints = layout.Exact(image.Pt(panelW, panelH))

	headerColor := color.NRGBA{R: 140, G: 160, B: 200, A: 255}
	lightGray := color.NRGBA{R: 220, G: 220, B: 220, A: 255}
	dimGray := color.NRGBA{R: 160, G: 160, B: 160, A: 255}
	accentGreen := color.NRGBA{R: 100, G: 220, B: 140, A: 255}

	curSizeIdx := p.sizeIdx(currentBytes)
	curRateIdx := p.rateIdx(currentInterval)

	layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// Header: available memory + time estimate
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			hdrInset := layout.Inset{
				Left:   unit.Dp(bufPanelHInsetDp),
				Right:  unit.Dp(bufPanelHInsetDp),
				Top:    unit.Dp(bufPanelHdrTopInsetDp),
				Bottom: unit.Dp(bufPanelDivVInsetDp),
			}

			return hdrInset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th, "Buffer Settings")
						lbl.Color = lightGray
						lbl.Font.Weight = font.Bold

						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						divInset := layout.Inset{Top: unit.Dp(dropdownDividerInsetDp)}

						return divInset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							txt := fmt.Sprintf("Available RAM: %s    |    Coverage: ≈ %s", availStr, formatDuration(coverage))
							lbl := material.Body2(th, txt)
							lbl.Color = accentGreen

							return lbl.Layout(gtx)
						})
					}),
				)
			})
		}),
		// Buffer size section
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			sizeHdrInset := layout.Inset{
				Left:   unit.Dp(bufPanelHInsetDp),
				Top:    unit.Dp(dropdownHeaderTopPadDp),
				Bottom: unit.Dp(bufPanelSectionVInsetDp),
			}

			return sizeHdrInset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, "BUFFER SIZE")
				lbl.Color = headerColor
				lbl.Font.Weight = font.Bold

				return lbl.Layout(gtx)
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			listStyle := material.List(th, &p.sizeList)
			nRows := len(bufSizePresets)

			return listStyle.Layout(gtx, nRows, func(gtx layout.Context, i int) layout.Dimensions {
				pr := bufSizePresets[i]

				if p.sizeItems[i].Clicked(gtx) {
					res.SizeChanged = true
					res.NewBytes = pr.Bytes
					p.Close()
				}

				isSelected := i == curSizeIdx
				isHovered := p.sizeItems[i].Hovered()

				// Annotate with warning if exceeds available
				label := pr.Label
				if avail > 0 && pr.Bytes > avail {
					label += "  (exceeds available)"
				}

				bgColor := color.NRGBA{A: 0}
				if isSelected {
					bgColor = color.NRGBA{R: 60, G: 90, B: 160, A: 255}
				} else if isHovered {
					bgColor = color.NRGBA{R: 55, G: 55, B: 55, A: 255}
				}

				return p.sizeItems[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Stack{}.Layout(gtx,
						layout.Expanded(func(gtx layout.Context) layout.Dimensions {
							if bgColor.A > 0 {
								defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
								paint.Fill(gtx.Ops, bgColor)
							}

							return layout.Dimensions{Size: gtx.Constraints.Min}
						}),
						layout.Stacked(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{
								Left: unit.Dp(bufPanelItemLInsetDp), Right: unit.Dp(bufPanelItemRInsetDp),
								Top: unit.Dp(bufPanelItemVInsetDp), Bottom: unit.Dp(bufPanelItemVInsetDp),
							}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								lbl := material.Body2(th, label)
								if avail > 0 && pr.Bytes > avail {
									lbl.Color = dimGray
								} else {
									lbl.Color = lightGray
								}
								if isSelected {
									lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
								}

								return lbl.Layout(gtx)
							})
						}),
					)
				})
			})
		}),
		// Divider
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			divInset := layout.Inset{
				Left:   unit.Dp(bufPanelHInsetDp),
				Right:  unit.Dp(bufPanelHInsetDp),
				Top:    unit.Dp(bufPanelDivVInsetDp),
				Bottom: unit.Dp(bufPanelDivVInsetDp),
			}

			return divInset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(1)))
				defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
				paint.Fill(gtx.Ops, color.NRGBA{R: 70, G: 70, B: 70, A: 255})

				return layout.Dimensions{Size: size}
			})
		}),
		// Sample rate section
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			rateHdrInset := layout.Inset{
				Left:   unit.Dp(bufPanelHInsetDp),
				Top:    unit.Dp(bufPanelSectionVInsetDp),
				Bottom: unit.Dp(bufPanelSectionVInsetDp),
			}

			return rateHdrInset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th, "SAMPLE RATE")
				lbl.Color = headerColor
				lbl.Font.Weight = font.Bold

				return lbl.Layout(gtx)
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			listStyle := material.List(th, &p.rateList)
			nRows := len(sampleRatePresets)

			return listStyle.Layout(gtx, nRows, func(gtx layout.Context, i int) layout.Dimensions {
				pr := sampleRatePresets[i]

				if p.rateItems[i].Clicked(gtx) {
					res.IntervalChanged = true
					res.NewInterval = pr.Interval
					p.Close()
				}

				isSelected := i == curRateIdx
				isHovered := p.rateItems[i].Hovered()

				bgColor := color.NRGBA{A: 0}
				if isSelected {
					bgColor = color.NRGBA{R: 60, G: 90, B: 160, A: 255}
				} else if isHovered {
					bgColor = color.NRGBA{R: 55, G: 55, B: 55, A: 255}
				}

				// Compute coverage at this rate
				eff := pr.Interval
				if eff == 0 {
					eff = defaultCameraInterval
				}
				cov := time.Duration(maxFrames) * eff
				covLabel := formatDuration(cov)

				return p.rateItems[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Stack{}.Layout(gtx,
						layout.Expanded(func(gtx layout.Context) layout.Dimensions {
							if bgColor.A > 0 {
								defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
								paint.Fill(gtx.Ops, bgColor)
							}

							return layout.Dimensions{Size: gtx.Constraints.Min}
						}),
						layout.Stacked(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{
								Left: unit.Dp(bufPanelItemLInsetDp), Right: unit.Dp(bufPanelItemRInsetDp),
								Top: unit.Dp(bufPanelItemVInsetDp), Bottom: unit.Dp(bufPanelItemVInsetDp),
							}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceBetween}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										lbl := material.Body2(th, pr.Label)
										lbl.Color = lightGray
										if isSelected {
											lbl.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
										}

										return lbl.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										lbl := material.Body2(th, "≈ "+covLabel)
										lbl.Color = dimGray
										if isSelected {
											lbl.Color = color.NRGBA{R: 200, G: 200, B: 255, A: 255}
										}
										lbl.Alignment = text.End

										return lbl.Layout(gtx)
									}),
								)
							})
						}),
					)
				})
			})
		}),
	)

	// Border
	borderClip := clip.Stroke{
		Path:  clip.Rect{Max: image.Pt(panelW, panelH)}.Path(),
		Width: float32(gtx.Dp(unit.Dp(1))),
	}.Op().Push(gtx.Ops)
	paint.Fill(gtx.Ops, color.NRGBA{R: 80, G: 80, B: 80, A: 255})
	borderClip.Pop()

	s.Pop()

	return res
}

// humanSizeDropdown formats bytes as human-readable (for use inside dropdown).
func humanSizeDropdown(b int64) string {
	const (
		mb = 1 << 20
		gb = 1 << 30
	)
	if b >= gb {
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	}

	return fmt.Sprintf("%d MB", b/mb)
}

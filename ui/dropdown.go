package ui

import (
	"fmt"
	"image"
	"image/color"

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
	itemH := gtx.Dp(unit.Dp(26))
	dropW := gtx.Dp(unit.Dp(280))
	maxVisibleItems := 16
	nRows := len(d.rows)
	visibleRows := nRows
	if visibleRows > maxVisibleItems {
		visibleRows = maxVisibleItems
	}
	dropH := itemH * visibleRows

	// Position: bottom-center of window (above status bar)
	x := (gtx.Constraints.Max.X - dropW) / 2
	y := gtx.Constraints.Max.Y - dropH - gtx.Dp(unit.Dp(40))
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
				Left: unit.Dp(8), Right: unit.Dp(8),
				Top: unit.Dp(6), Bottom: unit.Dp(2),
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
						Left: unit.Dp(16), Right: unit.Dp(10),
						Top: unit.Dp(3), Bottom: unit.Dp(3),
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

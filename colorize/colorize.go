// Package colorize converts raw thermal data into colorized RGBA images.
package colorize

import (
	"image"
	"image/color"
	"math"
	"sort"

	"thermalapp/camera"
)

const (
	percentileHighBound = 99  // upper percentile for AGCPercentile mode
	maxLUTIndex         = 255 // highest valid index into a 256-entry LUT
	lutSize             = 256 // number of entries in a color LUT
	rot180Steps         = 2   // 180° rotation in 90° steps
	rot270Steps         = 3   // 270° rotation in 90° steps
)

// AGCMode selects how the thermal data is normalized to 0-255.
type AGCMode int

const (
	AGCHardware   AGCMode = iota // Use IR brightness plane directly
	AGCPercentile                // 1st/99th percentile (default)
	AGCFixed                     // User-defined min/max
)

// Palette identifies a color LUT.
type Palette int

const (
	PaletteInferno Palette = iota
	PaletteIron
	PaletteJet
	PaletteGrayscale
	paletteCount
)

var paletteNames = [paletteCount]string{"Inferno", "Iron", "Jet", "Grayscale"}

func (p Palette) String() string { return paletteNames[p] }

// Next cycles to the next palette.
func (p Palette) Next() Palette { return (p + 1) % paletteCount }

var paletteLUTs = [paletteCount]*[256][3]uint8{
	&InfernoLUT,
	&IronLUT,
	&JetLUT,
	&GrayscaleLUT,
}

// LUT returns the color lookup table for this palette.
func (p Palette) LUT() *[256][3]uint8 { return paletteLUTs[p] }

// Params controls the colorization pipeline.
type Params struct {
	Mode    AGCMode
	Palette Palette
	// FixedMin/FixedMax are used when Mode == AGCFixed (in °C).
	FixedMin float32
	FixedMax float32
	// Emissivity for global surface correction (0 < ε ≤ 1).
	// Default 1.0 means no correction (blackbody assumption).
	Emissivity float32
}

// Result holds the colorized output and statistics.
type Result struct {
	RGBA       *image.RGBA
	MinC       float32 // min temperature in frame (°C)
	MaxC       float32 // max temperature in frame (°C)
	MinX, MinY int     // pixel position of min temp
	MaxX, MaxY int     // pixel position of max temp
	Width      int
	// Celsius holds per-pixel temps for cursor lookup (same layout as frame).
	// These are already corrected for the global emissivity.
	Celsius []float32
	// AmbientC is the estimated reflected/ambient temp used for emissivity correction.
	AmbientC float32
	// GlobalEmissivity is the emissivity applied to Celsius values.
	GlobalEmissivity float32
}

// Colorize converts a camera Frame into a colorized RGBA image.
func Colorize(frame *camera.Frame, params Params) *Result {
	width, height := frame.Width, frame.Height
	result := &Result{
		RGBA:  image.NewRGBA(image.Rect(0, 0, width, height)),
		Width: width,
	}
	lut := params.Palette.LUT()

	if params.Mode == AGCHardware {
		colorizeHardwareMode(frame, params, lut, result)

		return result
	}

	// Convert thermal data to celsius
	count := len(frame.Thermal)
	celsius := make([]float32, count)
	for idx := range frame.Thermal {
		celsius[idx] = frame.ToCelsiusAt(idx)
	}

	applyGlobalEmissivity(celsius, params.Emissivity, result)
	result.Celsius = celsius
	updateMinMax(celsius, width, result)

	var low, high float32
	switch params.Mode {
	case AGCPercentile:
		low, high = percentileBounds(celsius, 1, percentileHighBound)
	case AGCFixed:
		low, high = params.FixedMin, params.FixedMax
	}

	if high <= low {
		high = low + 1 // prevent division by zero
	}

	span := high - low
	for row := range height {
		for col := range width {
			idx := row*width + col
			norm := (celsius[idx] - low) / span
			if norm < 0 {
				norm = 0
			} else if norm > 1 {
				norm = 1
			}
			lutIdx := uint8(norm * maxLUTIndex)
			rgb := lut[lutIdx]
			result.RGBA.SetRGBA(col, row, color.RGBA{rgb[0], rgb[1], rgb[2], 255})
		}
	}

	return result
}

// colorizeHardwareMode fills result using the IR brightness plane directly.
func colorizeHardwareMode(frame *camera.Frame, params Params, lut *[256][3]uint8, result *Result) {
	width, height := frame.Width, frame.Height

	for row := range height {
		for col := range width {
			idx := row*width + col
			val := frame.IR[idx]
			rgb := lut[val]
			result.RGBA.SetRGBA(col, row, color.RGBA{rgb[0], rgb[1], rgb[2], 255})
		}
	}

	result.Celsius = make([]float32, len(frame.Thermal))
	for idx := range frame.Thermal {
		result.Celsius[idx] = frame.ToCelsiusAt(idx)
	}

	applyGlobalEmissivity(result.Celsius, params.Emissivity, result)
	updateMinMax(result.Celsius, width, result)
}

// applyGlobalEmissivity corrects celsius in-place for params.Emissivity and
// records AmbientC and GlobalEmissivity on result.
func applyGlobalEmissivity(celsius []float32, eps float32, result *Result) {
	if eps > 0 && eps < 1.0 {
		tRefl := EstimateAmbient(celsius)
		result.AmbientC = tRefl
		for idx := range celsius {
			celsius[idx] = CorrectEmissivity(celsius[idx], tRefl, eps)
		}
	}

	result.GlobalEmissivity = eps
}

// updateMinMax scans celsius and fills result.MinC/MaxC and their pixel positions.
func updateMinMax(celsius []float32, width int, result *Result) {
	if len(celsius) == 0 {
		return
	}

	result.MinC = celsius[0]
	result.MaxC = celsius[0]

	for idx, cel := range celsius {
		if cel < result.MinC {
			result.MinC = cel
			result.MinX = idx % width
			result.MinY = idx / width
		}
		if cel > result.MaxC {
			result.MaxC = cel
			result.MaxX = idx % width
			result.MaxY = idx / width
		}
	}
}

// percentileBounds returns the p_low and p_high percentile values from data.
func percentileBounds(data []float32, pLow, pHigh float64) (float32, float32) {
	count := len(data)
	if count == 0 {
		return 0, 1
	}

	// Sort a copy
	sorted := make([]float32, count)
	copy(sorted, data)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	lo := sorted[int(math.Floor(pLow/100*float64(count-1)))]
	hi := sorted[int(math.Ceil(pHigh/100*float64(count-1)))]

	return lo, hi
}

// MakeColorbar creates a 256×h RGBA image showing the current palette gradient.
func MakeColorbar(p Palette, h int) *image.RGBA {
	lut := p.LUT()
	img := image.NewRGBA(image.Rect(0, 0, lutSize, h))
	for x := range lutSize {
		c := lut[x]
		for y := range h {
			img.SetRGBA(x, y, color.RGBA{c[0], c[1], c[2], 255})
		}
	}

	return img
}

// rotateCoord maps a source pixel (srcCol, srcRow) to its destination (dstX, dstY)
// for a clockwise rotation of `steps` × 90° within an srcW × srcH image.
func rotateCoord(srcCol, srcRow, steps, srcW, srcH int) (dstX, dstY int) {
	switch steps {
	case 1: // 90° CW
		return srcH - 1 - srcRow, srcCol
	case rot180Steps: // 180°
		return srcW - 1 - srcCol, srcH - 1 - srcRow
	default: // 270° CW (steps == 3)
		return srcRow, srcW - 1 - srcCol
	}
}

// Rotate returns a new Result rotated 90° clockwise `steps` times (0-3).
// The RGBA image, Celsius array, dimensions, and min/max coordinates are all
// transformed so that downstream code can treat the result as an upright image.
func (r *Result) Rotate(steps int) *Result {
	steps %= 4
	if steps == 0 || r.RGBA == nil {
		return r
	}

	srcW := r.RGBA.Bounds().Dx()
	srcH := r.RGBA.Bounds().Dy()

	var dstW, dstH int
	if steps%2 == 0 {
		dstW, dstH = srcW, srcH
	} else {
		dstW, dstH = srcH, srcW
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	celsius := make([]float32, len(r.Celsius))

	for srcRow := range srcH {
		for srcCol := range srcW {
			destX, destY := rotateCoord(srcCol, srcRow, steps, srcW, srcH)
			dst.SetRGBA(destX, destY, r.RGBA.RGBAAt(srcCol, srcRow))
			srcIdx := srcRow*srcW + srcCol
			dstIdx := destY*dstW + destX
			if srcIdx < len(r.Celsius) && dstIdx < len(celsius) {
				celsius[dstIdx] = r.Celsius[srcIdx]
			}
		}
	}

	minX, minY := rotateCoord(r.MinX, r.MinY, steps, srcW, srcH)
	maxX, maxY := rotateCoord(r.MaxX, r.MaxY, steps, srcW, srcH)

	return &Result{
		RGBA:    dst,
		MinC:    r.MinC,
		MaxC:    r.MaxC,
		MinX:    minX,
		MinY:    minY,
		MaxX:    maxX,
		MaxY:    maxY,
		Width:   dstW,
		Celsius: celsius,
	}
}

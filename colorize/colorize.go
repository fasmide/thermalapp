// Package colorize converts raw thermal data into colorized RGBA images.
package colorize

import (
	"image"
	"image/color"
	"math"
	"sort"

	"thermalapp/camera"
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
	Celsius []float32
}

// Colorize converts a camera Frame into a colorized RGBA image.
func Colorize(frame *camera.Frame, params Params) *Result {
	w, h := frame.Width, frame.Height
	result := &Result{
		RGBA:  image.NewRGBA(image.Rect(0, 0, w, h)),
		Width: w,
	}
	lut := params.Palette.LUT()

	if params.Mode == AGCHardware {
		// Use the IR brightness plane directly
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				idx := y*w + x
				v := frame.IR[idx]
				c := lut[v]
				result.RGBA.SetRGBA(x, y, color.RGBA{c[0], c[1], c[2], 255})
			}
		}
		// Still compute celsius for cursor readout
		result.Celsius = make([]float32, len(frame.Thermal))
		for i, raw := range frame.Thermal {
			result.Celsius[i] = camera.ToCelsius(raw)
		}
		if len(result.Celsius) > 0 {
			result.MinC = result.Celsius[0]
			result.MaxC = result.Celsius[0]
			for i, c := range result.Celsius {
				if c < result.MinC {
					result.MinC = c
					result.MinX = i % w
					result.MinY = i / w
				}
				if c > result.MaxC {
					result.MaxC = c
					result.MaxX = i % w
					result.MaxY = i / w
				}
			}
		}
		return result
	}

	// Convert thermal data to celsius
	n := len(frame.Thermal)
	celsius := make([]float32, n)
	for i, raw := range frame.Thermal {
		celsius[i] = camera.ToCelsius(raw)
	}
	result.Celsius = celsius

	// Find min/max
	result.MinC = celsius[0]
	result.MaxC = celsius[0]
	for i, c := range celsius {
		if c < result.MinC {
			result.MinC = c
			result.MinX = i % w
			result.MinY = i / w
		}
		if c > result.MaxC {
			result.MaxC = c
			result.MaxX = i % w
			result.MaxY = i / w
		}
	}

	var low, high float32
	switch params.Mode {
	case AGCPercentile:
		low, high = percentileBounds(celsius, 1, 99)
	case AGCFixed:
		low, high = params.FixedMin, params.FixedMax
	}

	if high <= low {
		high = low + 1 // prevent division by zero
	}

	span := high - low
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			norm := (celsius[idx] - low) / span
			if norm < 0 {
				norm = 0
			} else if norm > 1 {
				norm = 1
			}
			lutIdx := uint8(norm * 255)
			c := lut[lutIdx]
			result.RGBA.SetRGBA(x, y, color.RGBA{c[0], c[1], c[2], 255})
		}
	}

	return result
}

// percentileBounds returns the p_low and p_high percentile values from data.
func percentileBounds(data []float32, pLow, pHigh float64) (float32, float32) {
	n := len(data)
	if n == 0 {
		return 0, 1
	}

	// Sort a copy
	sorted := make([]float32, n)
	copy(sorted, data)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	lo := sorted[int(math.Floor(pLow/100*float64(n-1)))]
	hi := sorted[int(math.Ceil(pHigh/100*float64(n-1)))]
	return lo, hi
}

// MakeColorbar creates a 256×h RGBA image showing the current palette gradient.
func MakeColorbar(p Palette, h int) *image.RGBA {
	lut := p.LUT()
	img := image.NewRGBA(image.Rect(0, 0, 256, h))
	for x := 0; x < 256; x++ {
		c := lut[x]
		for y := 0; y < h; y++ {
			img.SetRGBA(x, y, color.RGBA{c[0], c[1], c[2], 255})
		}
	}
	return img
}

// Rotate returns a new Result rotated 90° clockwise `steps` times (0-3).
// The RGBA image, Celsius array, dimensions, and min/max coordinates are all
// transformed so that downstream code can treat the result as an upright image.
func (r *Result) Rotate(steps int) *Result {
	steps = steps % 4
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

	for sy := 0; sy < srcH; sy++ {
		for sx := 0; sx < srcW; sx++ {
			var dx, dy int
			switch steps {
			case 1: // 90° CW
				dx, dy = srcH-1-sy, sx
			case 2: // 180°
				dx, dy = srcW-1-sx, srcH-1-sy
			case 3: // 270° CW
				dx, dy = sy, srcW-1-sx
			}
			dst.SetRGBA(dx, dy, r.RGBA.RGBAAt(sx, sy))
			srcIdx := sy*srcW + sx
			dstIdx := dy*dstW + dx
			if srcIdx < len(r.Celsius) && dstIdx < len(celsius) {
				celsius[dstIdx] = r.Celsius[srcIdx]
			}
		}
	}

	// Rotate min/max coords
	rotX := func(x, y int) int {
		switch steps {
		case 1:
			return srcH - 1 - y
		case 2:
			return srcW - 1 - x
		case 3:
			return y
		}
		return x
	}
	rotY := func(x, y int) int {
		switch steps {
		case 1:
			return x
		case 2:
			return srcH - 1 - y
		case 3:
			return srcW - 1 - x
		}
		return y
	}

	return &Result{
		RGBA:    dst,
		MinC:    r.MinC,
		MaxC:    r.MaxC,
		MinX:    rotX(r.MinX, r.MinY),
		MinY:    rotY(r.MinX, r.MinY),
		MaxX:    rotX(r.MaxX, r.MaxY),
		MaxY:    rotY(r.MaxX, r.MaxY),
		Width:   dstW,
		Celsius: celsius,
	}
}

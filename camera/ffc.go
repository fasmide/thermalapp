package camera

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

// Additional FFC (flat-field correction) constants.
const (
	// ffcBias is added before subtracting the FFC image so the result stays
	// positive for typical sensor values.  Matches the reference C++ code.
	ffcBias = 0x4000

	// irNormMax is the maximum 8-bit value for IR normalization.
	irNormMax = 255
)

// FlatFieldCorrector applies an additional flat-field correction image to
// every frame.  This removes fixed-pattern sensor gradient / vignetting
// that the per-frame shutter FFC cannot eliminate.
//
// The correction is: thermal[i] += ffcBias - ffc[i]
//
// It is camera-agnostic: any Camera implementation can benefit.
type FlatFieldCorrector struct {
	ffc    []uint16 // correction image (same dimensions as frame)
	width  int
	height int
}

// LoadFlatField reads a 16-bit grayscale PNG and returns a FlatFieldCorrector.
func LoadFlatField(path string) (*FlatFieldCorrector, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open flat-field file: %w", err)
	}
	defer file.Close()

	img, err := png.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("decode flat-field PNG: %w", err)
	}

	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	count := width * height
	ffc := make([]uint16, count)

	for row := range height {
		for col := range width {
			r, _, _, _ := img.At(col+bounds.Min.X, row+bounds.Min.Y).RGBA()
			// color.RGBA returns pre-multiplied 16-bit values; for Gray16
			// the red channel equals the gray value.
			ffc[row*width+col] = uint16(r)
		}
	}

	return &FlatFieldCorrector{
		ffc:    ffc,
		width:  width,
		height: height,
	}, nil
}

// Apply corrects a frame in-place: thermal[i] += ffcBias - ffc[i].
// The IR plane is regenerated from the corrected thermal values.
// Returns an error if the frame dimensions do not match.
func (f *FlatFieldCorrector) Apply(frame *Frame) error {
	if frame.Width != f.width || frame.Height != f.height {
		return fmt.Errorf("flat-field size %dx%d does not match frame %dx%d",
			f.width, f.height, frame.Width, frame.Height)
	}

	count := len(frame.Thermal)
	if count != len(f.ffc) {
		return fmt.Errorf("flat-field pixel count %d does not match frame %d",
			len(f.ffc), count)
	}

	for idx := range count {
		val := int(frame.Thermal[idx]) + ffcBias - int(f.ffc[idx])
		if val < 0 {
			val = 0
		} else if val > math.MaxUint16 {
			val = math.MaxUint16
		}

		frame.Thermal[idx] = uint16(val)
	}

	// Regenerate IR from corrected thermal.
	frame.IR = linearStretchIR(frame.Thermal)

	return nil
}

// linearStretchIR converts 16-bit thermal to 8-bit IR via min/max linear stretch.
func linearStretchIR(thermal []uint16) []uint8 {
	count := len(thermal)
	result := make([]uint8, count)

	var tMin, tMax uint16 = math.MaxUint16, 0

	for _, val := range thermal {
		if val < tMin {
			tMin = val
		}

		if val > tMax {
			tMax = val
		}
	}

	if tMax <= tMin {
		return result
	}

	span := float64(tMax) - float64(tMin)

	for idx, val := range thermal {
		norm := (float64(val) - float64(tMin)) / span
		if norm < 0 {
			norm = 0
		} else if norm > 1 {
			norm = 1
		}

		result[idx] = uint8(norm * irNormMax)
	}

	return result
}

// FlatFieldAccumulator collects corrected frames and produces an averaged
// flat-field calibration image.
type FlatFieldAccumulator struct {
	sum    []float64 // running sum of thermal values
	count  int       // number of frames accumulated
	width  int
	height int
}

// NewFlatFieldAccumulator creates an accumulator for the given frame dimensions.
func NewFlatFieldAccumulator(width, height int) *FlatFieldAccumulator {
	return &FlatFieldAccumulator{
		sum:    make([]float64, width*height),
		width:  width,
		height: height,
	}
}

// Add accumulates one frame's thermal data.
func (a *FlatFieldAccumulator) Add(frame *Frame) error {
	if frame.Width != a.width || frame.Height != a.height {
		return fmt.Errorf("frame size %dx%d does not match accumulator %dx%d",
			frame.Width, frame.Height, a.width, a.height)
	}

	for idx, val := range frame.Thermal {
		a.sum[idx] += float64(val)
	}

	a.count++

	return nil
}

// Count returns the number of frames accumulated so far.
func (a *FlatFieldAccumulator) Count() int {
	return a.count
}

// Save writes the averaged flat-field image as a 16-bit grayscale PNG.
func (a *FlatFieldAccumulator) Save(path string) error {
	if a.count == 0 {
		return fmt.Errorf("no frames accumulated")
	}

	img := image.NewGray16(image.Rect(0, 0, a.width, a.height))

	divisor := float64(a.count)

	for row := range a.height {
		for col := range a.width {
			idx := row*a.width + col
			avg := a.sum[idx] / divisor

			if avg < 0 {
				avg = 0
			} else if avg > math.MaxUint16 {
				avg = math.MaxUint16
			}

			img.SetGray16(col, row, color.Gray16{Y: uint16(avg)})
		}
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer file.Close()

	if err := png.Encode(file, img); err != nil {
		return fmt.Errorf("encode PNG: %w", err)
	}

	return nil
}

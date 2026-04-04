package camera

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
)

// ffcFileHeaderSize is the total byte size of the .ffc binary file header.
const ffcFileHeaderSize = 12

// ffcFileMagic identifies flat-field correction files produced by this application.
var ffcFileMagic = [8]byte{'T', 'H', 'A', 'F', 'F', '1', 0, 0}

// irNormMax is the maximum 8-bit value for IR normalization.
const irNormMax = 255

// FlatFieldCorrector applies a per-pixel temperature correction to every frame.
// The correction removes fixed-pattern sensor gradients and vignetting that
// the per-frame shutter FFC cannot eliminate.
//
// The correction is loaded from a .ffc binary file produced by FlatFieldAccumulator.
// For each pixel: celsius_corrected[i] = celsius[i] + correction[i].
//
// It is camera-agnostic: any Camera implementation can benefit.
type FlatFieldCorrector struct {
	correction []float32 // per-pixel additive correction in °C (global_mean − per_pixel_avg)
	width      int
	height     int
}

// LoadFlatField reads a .ffc binary file and returns a FlatFieldCorrector.
func LoadFlatField(path string) (*FlatFieldCorrector, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open flat-field file: %w", err)
	}
	defer file.Close()

	var hdr [ffcFileHeaderSize]byte
	if _, err := io.ReadFull(file, hdr[:]); err != nil {
		return nil, fmt.Errorf("read flat-field header: %w", err)
	}

	if [8]byte(hdr[:8]) != ffcFileMagic {
		return nil, fmt.Errorf("not a .ffc file (bad magic)")
	}

	width := int(binary.LittleEndian.Uint16(hdr[8:10]))
	height := int(binary.LittleEndian.Uint16(hdr[10:12]))
	count := width * height
	correction := make([]float32, count)

	for pixIdx := range count {
		var bits [4]byte
		if _, err := io.ReadFull(file, bits[:]); err != nil {
			return nil, fmt.Errorf("read flat-field pixel %d: %w", pixIdx, err)
		}

		correction[pixIdx] = math.Float32frombits(binary.LittleEndian.Uint32(bits[:]))
	}

	return &FlatFieldCorrector{
		correction: correction,
		width:      width,
		height:     height,
	}, nil
}

// Apply corrects a frame in-place: celsius[i] += correction[i].
// The IR plane is regenerated from the corrected Celsius values.
// Returns an error if the frame dimensions do not match.
func (f *FlatFieldCorrector) Apply(frame *Frame) error {
	if frame.Width != f.width || frame.Height != f.height {
		return fmt.Errorf("flat-field size %dx%d does not match frame %dx%d",
			f.width, f.height, frame.Width, frame.Height)
	}

	if len(frame.Celsius) != len(f.correction) {
		return fmt.Errorf("flat-field pixel count %d does not match frame %d",
			len(f.correction), len(frame.Celsius))
	}

	for i := range frame.Celsius {
		frame.Celsius[i] += f.correction[i]
	}

	// Regenerate IR from corrected Celsius.
	frame.IR = linearStretchIR(frame.Celsius)

	return nil
}

// linearStretchIR converts a float32 Celsius slice to 8-bit IR via min/max
// linear stretch. Monotonic in temperature, so the result is equivalent to
// stretching raw thermal counts.
func linearStretchIR(celsius []float32) []uint8 {
	count := len(celsius)
	result := make([]uint8, count)

	if count == 0 {
		return result
	}

	tMin, tMax := celsius[0], celsius[0]

	for _, val := range celsius {
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

	span := tMax - tMin

	for idx, val := range celsius {
		norm := (val - tMin) / span
		if norm < 0 {
			norm = 0
		} else if norm > 1 {
			norm = 1
		}

		result[idx] = uint8(norm * irNormMax)
	}

	return result
}

// FlatFieldAccumulator collects frames and produces an averaged flat-field
// calibration file. Point the camera at a spatially uniform surface (e.g.
// a wall, clear sky, or blackbody source) while accumulating.
type FlatFieldAccumulator struct {
	sum    []float64 // running sum of per-pixel Celsius values
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

// Add accumulates one frame's Celsius data.
func (a *FlatFieldAccumulator) Add(frame *Frame) error {
	if frame.Width != a.width || frame.Height != a.height {
		return fmt.Errorf("frame size %dx%d does not match accumulator %dx%d",
			frame.Width, frame.Height, a.width, a.height)
	}

	for idx, val := range frame.Celsius {
		a.sum[idx] += float64(val)
	}

	a.count++

	return nil
}

// Count returns the number of frames accumulated so far.
func (a *FlatFieldAccumulator) Count() int {
	return a.count
}

// Save writes the flat-field correction to a .ffc binary file.
// The correction stored per pixel is global_mean − per_pixel_avg, so that
// applying it (adding) centres all pixels at the same temperature.
// A 16-bit grayscale PNG preview is also written alongside (same path + ".png").
func (a *FlatFieldAccumulator) Save(path string) error {
	if a.count == 0 {
		return fmt.Errorf("no frames accumulated")
	}

	count := a.width * a.height
	divisor := float64(a.count)

	avg := make([]float64, count)
	globalSum := 0.0

	for i, s := range a.sum {
		avg[i] = s / divisor
		globalSum += avg[i]
	}

	globalMean := globalSum / float64(count)

	// Build correction: for each pixel, how much to add to reach the global mean.
	correction := make([]float32, count)
	for i := range count {
		correction[i] = float32(globalMean - avg[i])
	}

	if err := a.writeFFCFile(path, correction); err != nil {
		return err
	}

	return a.writePNGPreview(path+".png", avg)
}

// writeFFCFile writes the binary .ffc correction file.
func (a *FlatFieldAccumulator) writeFFCFile(path string, correction []float32) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create .ffc file: %w", err)
	}
	defer file.Close()

	var hdr [ffcFileHeaderSize]byte
	copy(hdr[:8], ffcFileMagic[:])
	binary.LittleEndian.PutUint16(hdr[8:10], uint16(a.width))   //nolint:gosec // dimensions fit uint16
	binary.LittleEndian.PutUint16(hdr[10:12], uint16(a.height)) //nolint:gosec

	if _, err := file.Write(hdr[:]); err != nil {
		return fmt.Errorf("write .ffc header: %w", err)
	}

	for _, c := range correction {
		var bits [4]byte
		binary.LittleEndian.PutUint32(bits[:], math.Float32bits(c))

		if _, err := file.Write(bits[:]); err != nil {
			return fmt.Errorf("write .ffc pixel: %w", err)
		}
	}

	return nil
}

// writePNGPreview writes a 16-bit grayscale PNG for human inspection.
// The average temperatures are scaled into the uint16 range for display.
func (a *FlatFieldAccumulator) writePNGPreview(path string, avg []float64) error {
	// Find min/max of averages for scaling.
	tMin, tMax := avg[0], avg[0]

	for _, val := range avg {
		if val < tMin {
			tMin = val
		}

		if val > tMax {
			tMax = val
		}
	}

	span := tMax - tMin
	img := image.NewGray16(image.Rect(0, 0, a.width, a.height))

	for row := range a.height {
		for col := range a.width {
			idx := row*a.width + col
			var scaled uint16

			if span > 0 {
				norm := (avg[idx] - tMin) / span
				scaled = uint16(norm * math.MaxUint16) //nolint:gosec // norm is in [0,1]
			}

			img.SetGray16(col, row, color.Gray16{Y: scaled})
		}
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create PNG preview: %w", err)
	}
	defer file.Close()

	if err := png.Encode(file, img); err != nil {
		return fmt.Errorf("encode PNG preview: %w", err)
	}

	return nil
}

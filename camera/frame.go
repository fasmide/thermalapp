package camera

// kelvinOffset is the offset between the Kelvin and Celsius temperature scales.
const kelvinOffset = 273.15

// Frame holds a decoded thermal camera frame.
// Both camera drivers always populate every field before returning.
type Frame struct {
	Width, Height int

	// IR holds per-pixel 8-bit AGC brightness (hardware-computed by the driver).
	IR []uint8

	// Celsius holds per-pixel temperature in °C, always populated by the driver.
	Celsius []float32

	// ShutterActive is true when the sensor shutter is closed (NUC calibration
	// in progress). Frames with ShutterActive set contain stale/invalid data.
	ShutterActive bool
}

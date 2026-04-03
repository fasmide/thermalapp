// Package camera defines a generic thermal camera interface and the P3 USB implementation.
package camera

import (
	"fmt"
	"image"
	"os"
	"strings"
)

// GainMode represents the sensor gain setting.
type GainMode int

const (
	GainHigh GainMode = iota // -20°C to 150°C, higher sensitivity
	GainLow                  // 0°C to 550°C, extended range
)

// Frame holds a decoded thermal camera frame.
type Frame struct {
	// Thermal contains raw uint16 thermal values.
	// The encoding depends on the camera:
	//   P3:   absolute temperature in 1/64 Kelvin
	//   Seek: relative post-FFC values centered at 0x4000
	// Use Frame.ToCelsius() for camera-agnostic conversion.
	Thermal []uint16

	// IR contains 8-bit hardware-AGC brightness values.
	// Dimensions: SensorH x SensorW.
	IR []uint8

	// Metadata holds the 2 raw metadata rows (available during live capture;
	// not stored in recordings).
	Metadata []uint16

	// Width and Height of the thermal/IR image.
	Width, Height int

	// ShutterActive is true when the sensor shutter is closed (NUC calibration
	// in progress). Frames with ShutterActive=true contain stale/invalid data.
	ShutterActive bool

	// HardwareFrameCounter is the camera's internal frame counter (metadata
	// register 64). It freezes during NUC — that is how ShutterActive is
	// determined.
	HardwareFrameCounter uint16

	// Celsius holds per-pixel temperatures in °C, pre-computed by the camera
	// driver. When non-nil, ToCelsiusAt returns Celsius[idx] directly without
	// consulting Thermal[], CelsiusLUT, or the linear formula. Cameras that
	// use non-trivial per-pixel models (e.g. Seek v5 TLUT) populate this at
	// read time so that callers need no knowledge of the camera-specific formula.
	Celsius []float32
}

const (
	// rawThermalScale is the divisor converting raw thermal units to Kelvin (1/64 K per LSB).
	rawThermalScale = 64.0
	// kelvinOffset is the offset from Kelvin to Celsius.
	kelvinOffset = 273.15

	// p3CelsiusPerCount is the P3 thermal scale: 1/64 degree K per raw count.
	p3CelsiusPerCount = 1.0 / rawThermalScale
	// p3CelsiusBase converts the P3 zero-point from Kelvin to Celsius.
	p3CelsiusBase = -kelvinOffset
)

// ToCelsius converts a raw thermal value to degrees Celsius using the P3
// linear formula (1/64 K per count, offset by −273.15).
//
// Prefer ToCelsiusAt when iterating over pixels — it returns pre-computed
// Celsius values for cameras that populate Frame.Celsius (e.g. Seek v5).
func (f *Frame) ToCelsius(raw uint16) float32 {
	return float32(raw)*p3CelsiusPerCount + p3CelsiusBase
}

// ToCelsiusAt returns the temperature in °C for the pixel at index idx.
// When Frame.Celsius is non-nil (pre-computed by the camera driver) it is
// returned directly. Otherwise falls back to ToCelsius(Thermal[idx]).
func (f *Frame) ToCelsiusAt(idx int) float32 {
	if f.Celsius != nil {
		return f.Celsius[idx]
	}

	return f.ToCelsius(f.Thermal[idx])
}

// Camera is the interface for a thermal camera device.
// Designed to be implemented by different camera models (P3, P1, etc.).
type Camera interface {
	// Connect finds and opens the USB device.
	Connect() error

	// Init performs the device initialization handshake and returns device info.
	Init() (DeviceInfo, error)

	// StartStreaming begins the frame streaming pipeline.
	StartStreaming() error

	// ReadFrame reads and parses one complete frame from the device.
	ReadFrame() (*Frame, error)

	// StopStreaming halts frame streaming.
	StopStreaming() error

	// Close releases all USB resources.
	Close()

	// TriggerShutter sends a NUC/shutter calibration command.
	TriggerShutter() error

	// SetGain switches between high and low gain modes.
	SetGain(mode GainMode) error

	// SensorSize returns the native image dimensions (width, height).
	SensorSize() image.Point
}

// DeviceInfo holds identification data read from the camera.
type DeviceInfo struct {
	Model      string
	FWVersion  string
	PartNumber string
	Serial     string
	HWVersion  string
}

// usbDevPathHint scans sysfs to find the USB device path for the given VID/PID
// and returns a helpful "sudo chown" hint string. Returns empty if not found.
func usbDevPathHint(vid, pid uint16) string {
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return ""
	}

	vidStr := fmt.Sprintf("%04x", vid)
	pidStr := fmt.Sprintf("%04x", pid)

	for _, entry := range entries {
		base := "/sys/bus/usb/devices/" + entry.Name()

		devVID, _ := os.ReadFile(base + "/idVendor")
		devPID, _ := os.ReadFile(base + "/idProduct")

		if strings.TrimSpace(string(devVID)) != vidStr ||
			strings.TrimSpace(string(devPID)) != pidStr {
			continue
		}

		busnum, _ := os.ReadFile(base + "/busnum")
		devnum, _ := os.ReadFile(base + "/devnum")
		bus := strings.TrimSpace(string(busnum))
		dev := strings.TrimSpace(string(devnum))

		if bus != "" && dev != "" {
			return fmt.Sprintf("\n\nTry running:\n  sudo chown $USER /dev/bus/usb/%03s/%03s", bus, dev)
		}
	}

	return ""
}

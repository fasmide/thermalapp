// Package camera defines a generic thermal camera interface and the P3 USB implementation.
package camera

import (
	"image"
)

// GainMode represents the sensor gain setting.
type GainMode int

const (
	GainHigh GainMode = iota // -20°C to 150°C, higher sensitivity
	GainLow                  // 0°C to 550°C, extended range
)

// Frame holds a decoded thermal camera frame.
type Frame struct {
	// Thermal contains raw uint16 temperature values (1/64 Kelvin units).
	// Dimensions: SensorH x SensorW.
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
}

// ToCelsius converts a raw uint16 thermal value to degrees Celsius.
func ToCelsius(raw uint16) float32 {
	return (float32(raw) / 64.0) - 273.15
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

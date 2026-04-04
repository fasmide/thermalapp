// Package camera provides a generic thermal camera interface and per-camera
// driver implementations (Seek CompactPRO, P3).
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

// Camera is the interface implemented by every camera driver.
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

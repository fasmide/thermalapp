package camera

import (
	"bytes"
	"fmt"
	"image"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/gousb"
)

const (
	p3VID = 0x3474
	p3PID = 0x45A2

	chunkSize = 16384 // USB bulk read chunk size
)

// P3Camera drives the Thermal Master P3 USB thermal camera.
type P3Camera struct {
	ctx *gousb.Context
	dev *gousb.Device
	cfg *gousb.Config

	intf0 *gousb.Interface // control interface (0, alt 0)
	intf1 *gousb.Interface // streaming interface (1, alt 0 or 1)
	ep    *gousb.InEndpoint

	streaming bool
	gainMode  GainMode

	frameBuf []byte

	// Shutter detection: metadata register 64 is the camera's frame counter.
	// When it stops incrementing, the shutter is active (NUC in progress).
	prevFrameCnt uint16
	firstFrame   bool
}

var _ Camera = (*P3Camera)(nil)

func NewP3Camera() *P3Camera {
	return &P3Camera{gainMode: GainHigh, firstFrame: true}
}

func (c *P3Camera) SensorSize() image.Point {
	return image.Pt(p3SensorW, p3SensorH)
}

func (c *P3Camera) Connect() error {
	c.ctx = gousb.NewContext()

	dev, err := c.ctx.OpenDeviceWithVIDPID(p3VID, p3PID)
	if err != nil {
		// Find bus/address for a helpful error message
		hint := usbDevPath()
		c.ctx.Close()
		return fmt.Errorf("open P3 device: %w%s", err, hint)
	}
	if dev == nil {
		c.ctx.Close()
		return fmt.Errorf("P3 camera not found (VID=%04x PID=%04x)", p3VID, p3PID)
	}

	if err := dev.SetAutoDetach(true); err != nil {
		dev.Close()
		c.ctx.Close()
		return fmt.Errorf("set auto-detach: %w", err)
	}

	c.dev = dev

	cfg, err := dev.Config(1)
	if err != nil {
		dev.Close()
		c.ctx.Close()
		return fmt.Errorf("get config 1: %w", err)
	}
	c.cfg = cfg

	// Claim Interface 0 (control commands)
	intf0, err := cfg.Interface(0, 0)
	if err != nil {
		cfg.Close()
		dev.Close()
		c.ctx.Close()
		return fmt.Errorf("claim interface 0: %w", err)
	}
	c.intf0 = intf0

	// Claim Interface 1 alt 0 (streaming inactive)
	intf1, err := cfg.Interface(1, 0)
	if err != nil {
		intf0.Close()
		cfg.Close()
		dev.Close()
		c.ctx.Close()
		return fmt.Errorf("claim interface 1: %w", err)
	}
	c.intf1 = intf1

	c.frameBuf = make([]byte, p3FrameSize+2*markerSize)
	return nil
}

func (c *P3Camera) Init() (DeviceInfo, error) {
	var info DeviceInfo

	name, err := c.readRegister("read_name", 30)
	if err != nil {
		return info, fmt.Errorf("read name: %w", err)
	}
	info.Model = trimNull(name)

	ver, err := c.readRegister("read_version", 12)
	if err != nil {
		return info, fmt.Errorf("read version: %w", err)
	}
	info.FWVersion = trimNull(ver)

	pn, err := c.readRegister("read_part_number", 64)
	if err != nil {
		return info, fmt.Errorf("read part number: %w", err)
	}
	info.PartNumber = trimNull(pn)

	serial, err := c.readRegister("read_serial", 64)
	if err != nil {
		return info, fmt.Errorf("read serial: %w", err)
	}
	info.Serial = trimNull(serial)

	hw, err := c.readRegister("read_hw_version", 64)
	if err != nil {
		return info, fmt.Errorf("read hw version: %w", err)
	}
	info.HWVersion = trimNull(hw)

	return info, nil
}

func (c *P3Camera) StartStreaming() error {
	// Initial start_stream command with status checks
	if err := c.sendCommand(commands["start_stream"]); err != nil {
		return fmt.Errorf("start_stream cmd: %w", err)
	}
	c.readStatus()
	c.readResponse(1)
	c.readStatus()

	time.Sleep(1 * time.Second)

	// Switch Interface 1 to alt setting 1 (enables streaming).
	// Close current claim, re-claim with alt 1.
	c.intf1.Close()

	intf1, err := c.cfg.Interface(1, 1)
	if err != nil {
		return fmt.Errorf("set interface 1 alt 1: %w", err)
	}
	c.intf1 = intf1

	// Find bulk IN endpoint 0x81
	ep, err := intf1.InEndpoint(0x81)
	if err != nil {
		return fmt.Errorf("get endpoint 0x81: %w", err)
	}
	c.ep = ep

	// Send 0xEE control transfer (start streaming at device level)
	_, err = c.dev.Control(0x40, 0xEE, 0, 1, nil)
	if err != nil {
		return fmt.Errorf("send 0xEE: %w", err)
	}

	time.Sleep(2 * time.Second)

	// Attempt initial bulk read (may timeout — expected)
	tmpBuf := make([]byte, p3FrameSize)
	c.ep.Read(tmpBuf)

	// Final start_stream command
	if err := c.sendCommand(commands["start_stream"]); err != nil {
		return fmt.Errorf("final start_stream: %w", err)
	}
	c.readStatus()
	c.readResponse(1)
	c.readStatus()

	c.streaming = true
	return nil
}

func (c *P3Camera) ReadFrame() (*Frame, error) {
	if !c.streaming || c.ep == nil {
		return nil, fmt.Errorf("not streaming")
	}

	totalSize := p3FrameSize + 2*markerSize
	pos := 0
	chunk := make([]byte, chunkSize)

	for pos < totalSize {
		n, err := c.ep.Read(chunk)
		if err != nil {
			return nil, fmt.Errorf("bulk read: %w", err)
		}

		// Frame sync logic (from p3_camera.py read_frame):
		// End marker is always a 12-byte read. If we get 12 bytes
		// mid-frame, or exceed total without ending on 12 bytes, resync.
		nextPos := pos + n
		if (n == markerSize && nextPos < totalSize) || (nextPos >= totalSize && n != markerSize) {
			pos = 0
			log.Println("frame sync: dropping, resetting")
			continue
		}

		copy(c.frameBuf[pos:pos+n], chunk[:n])
		pos = nextPos
	}

	startMarker := parseMarker(c.frameBuf[:markerSize])
	endMarker := parseMarker(c.frameBuf[totalSize-markerSize : totalSize])

	if startMarker.Cnt1 != endMarker.Cnt1 {
		log.Printf("marker cnt1 mismatch: start=%d end=%d", startMarker.Cnt1, endMarker.Cnt1)
	}

	frame, err := ParseP3Frame(c.frameBuf[:markerSize+p3FrameSize])
	if err != nil {
		return nil, err
	}

	// Populate shutter state from metadata registers.
	// Register 64 = camera frame counter, register 72 = shutter countdown.
	if len(frame.Metadata) > 64 {
		frameCnt := frame.Metadata[64]
		frame.HardwareFrameCounter = frameCnt

		if c.firstFrame {
			c.firstFrame = false
		} else if frameCnt == c.prevFrameCnt {
			frame.ShutterActive = true
		}
		c.prevFrameCnt = frameCnt
	}

	return frame, nil
}

func (c *P3Camera) StopStreaming() error {
	if !c.streaming {
		return nil
	}

	c.intf1.Close()

	intf1, err := c.cfg.Interface(1, 0)
	if err != nil {
		return fmt.Errorf("set interface 1 alt 0: %w", err)
	}
	c.intf1 = intf1
	c.ep = nil
	c.streaming = false
	return nil
}

func (c *P3Camera) Close() {
	if c.streaming {
		c.StopStreaming()
	}
	if c.intf1 != nil {
		c.intf1.Close()
	}
	if c.intf0 != nil {
		c.intf0.Close()
	}
	if c.cfg != nil {
		c.cfg.Close()
	}
	if c.dev != nil {
		c.dev.Close()
	}
	if c.ctx != nil {
		c.ctx.Close()
	}
}

func (c *P3Camera) TriggerShutter() error {
	if err := c.sendCommand(commands["shutter"]); err != nil {
		return fmt.Errorf("shutter cmd: %w", err)
	}
	c.readStatus()
	return nil
}

func (c *P3Camera) SetGain(mode GainMode) error {
	var cmd []byte
	switch mode {
	case GainHigh:
		cmd = commands["gain_high"]
	case GainLow:
		cmd = commands["gain_low"]
	default:
		return fmt.Errorf("unknown gain mode: %d", mode)
	}

	if err := c.sendCommand(cmd); err != nil {
		return fmt.Errorf("gain cmd: %w", err)
	}
	c.readStatus()
	c.gainMode = mode
	return nil
}

// --- internal USB helpers ---

func (c *P3Camera) sendCommand(cmd []byte) error {
	_, err := c.dev.Control(
		0x41, // OUT | VENDOR | INTERFACE
		0x20,
		0, 0,
		cmd,
	)
	return err
}

func (c *P3Camera) readResponse(length int) ([]byte, error) {
	buf := make([]byte, length)
	n, err := c.dev.Control(0xC1, 0x21, 0, 0, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (c *P3Camera) readStatus() (byte, error) {
	buf := make([]byte, 1)
	_, err := c.dev.Control(0xC1, 0x22, 0, 0, buf)
	return buf[0], err
}

func (c *P3Camera) readRegister(cmdName string, length int) ([]byte, error) {
	if err := c.sendCommand(commands[cmdName]); err != nil {
		return nil, err
	}
	if _, err := c.readStatus(); err != nil {
		return nil, err
	}
	data, err := c.readResponse(length)
	if err != nil {
		return nil, err
	}
	if _, err := c.readStatus(); err != nil {
		return nil, err
	}
	return data, nil
}

func trimNull(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// usbDevPath scans sysfs to find the P3 camera's /dev/bus/usb path
// without needing permissions to open the device.
func usbDevPath() string {
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		base := "/sys/bus/usb/devices/" + e.Name()
		vid, _ := os.ReadFile(base + "/idVendor")
		pid, _ := os.ReadFile(base + "/idProduct")
		if strings.TrimSpace(string(vid)) == fmt.Sprintf("%04x", p3VID) &&
			strings.TrimSpace(string(pid)) == fmt.Sprintf("%04x", p3PID) {
			busnum, _ := os.ReadFile(base + "/busnum")
			devnum, _ := os.ReadFile(base + "/devnum")
			bus := strings.TrimSpace(string(busnum))
			dev := strings.TrimSpace(string(devnum))
			if bus != "" && dev != "" {
				return fmt.Sprintf("\n\nTry running:\n  sudo chown $USER /dev/bus/usb/%03s/%03s", bus, dev)
			}
		}
	}
	return ""
}

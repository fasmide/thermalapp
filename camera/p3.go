package camera

import (
	"bytes"
	"fmt"
	"image"
	"log"
	"time"

	"github.com/google/gousb"
)

const (
	p3VID = 0x3474
	p3PID = 0x45A2

	chunkSize = 16384 // USB bulk read chunk size

	// USB control-transfer bmRequestType values.
	usbCtrlVendorOut = 0x40 // host-to-device | vendor | interface
	usbCtrlVendorIn  = 0xC1 // device-to-host | vendor | interface
	usbCmdSend       = 0x41 // OUT | VENDOR | INTERFACE (used in sendCommand)

	// USB vendor command codes.
	usbBRequestSend        = 0x20 // send command bRequest
	usbBRequestResponse    = 0x21 // read response bRequest
	usbBRequestStatus      = 0x22 // read status bRequest
	usbBRequestStartStream = 0xEE // start streaming bRequest

	// Bulk IN endpoint address for streaming.
	usbStreamEndpoint = 0x81

	// Register buffer sizes used in Init().
	regNameLen      = 30
	regVersionLen   = 12
	regPartNumLen   = 64
	regSerialLen    = 64
	regHWVersionLen = 64

	// Metadata layout.
	metadataMaxIdx = 64 // register index of the camera frame counter

	// Startup timing delays.
	startStreamDelay1 = 1 * time.Second // delay before switching to alt interface
	startStreamDelay2 = 2 * time.Second // delay after sending 0xEE before bulk read

	// Frame boundary markers.
	markerCount = 2 // number of start/end markers surrounding each frame
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
		hint := usbDevPathHint(p3VID, p3PID)
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

	name, err := c.readRegister("read_name", regNameLen)
	if err != nil {
		return info, fmt.Errorf("read name: %w", err)
	}
	info.Model = trimNull(name)

	ver, err := c.readRegister("read_version", regVersionLen)
	if err != nil {
		return info, fmt.Errorf("read version: %w", err)
	}
	info.FWVersion = trimNull(ver)

	pn, err := c.readRegister("read_part_number", regPartNumLen)
	if err != nil {
		return info, fmt.Errorf("read part number: %w", err)
	}
	info.PartNumber = trimNull(pn)

	serial, err := c.readRegister("read_serial", regSerialLen)
	if err != nil {
		return info, fmt.Errorf("read serial: %w", err)
	}
	info.Serial = trimNull(serial)

	hw, err := c.readRegister("read_hw_version", regHWVersionLen)
	if err != nil {
		return info, fmt.Errorf("read hw version: %w", err)
	}
	info.HWVersion = trimNull(hw)

	return info, nil
}

// startStreamHandshake sends the start_stream command and performs the
// surrounding status/response checks (two round trips).
func (c *P3Camera) startStreamHandshake(label string) error {
	if err := c.sendCommand(commands["start_stream"]); err != nil {
		return fmt.Errorf("%s cmd: %w", label, err)
	}
	if err := c.readStatus(); err != nil {
		return fmt.Errorf("%s status 1: %w", label, err)
	}
	if _, err := c.readResponse(1); err != nil {
		return fmt.Errorf("%s response: %w", label, err)
	}
	if err := c.readStatus(); err != nil {
		return fmt.Errorf("%s status 2: %w", label, err)
	}

	return nil
}

// openStreamEndpoint switches Interface 1 to alt-setting 1 and opens the
// bulk IN endpoint, making the camera ready to stream.
func (c *P3Camera) openStreamEndpoint() error {
	c.intf1.Close()

	intf1, err := c.cfg.Interface(1, 1)
	if err != nil {
		return fmt.Errorf("set interface 1 alt 1: %w", err)
	}
	c.intf1 = intf1

	ep, err := intf1.InEndpoint(usbStreamEndpoint)
	if err != nil {
		return fmt.Errorf("get endpoint 0x81: %w", err)
	}
	c.ep = ep

	_, err = c.dev.Control(usbCtrlVendorOut, usbBRequestStartStream, 0, 1, nil)
	if err != nil {
		return fmt.Errorf("send 0xEE: %w", err)
	}

	return nil
}

func (c *P3Camera) StartStreaming() error {
	if err := c.startStreamHandshake("start_stream"); err != nil {
		return err
	}

	time.Sleep(startStreamDelay1)

	if err := c.openStreamEndpoint(); err != nil {
		return err
	}

	time.Sleep(startStreamDelay2)

	// Attempt initial bulk read (may timeout — expected)
	tmpBuf := make([]byte, p3FrameSize)
	if _, err := c.ep.Read(tmpBuf); err != nil {
		log.Printf("initial bulk read (expected timeout): %v", err)
	}

	if err := c.startStreamHandshake("final start_stream"); err != nil {
		return err
	}

	c.streaming = true

	return nil
}

// updateShutterState updates frame.ShutterActive and frame.HardwareFrameCounter
// based on metadata register 64, and advances the camera's prevFrameCnt.
func (c *P3Camera) updateShutterState(frame *Frame) {
	if len(frame.Metadata) <= metadataMaxIdx {
		return
	}

	frameCnt := frame.Metadata[metadataMaxIdx]
	frame.HardwareFrameCounter = frameCnt

	if c.firstFrame {
		c.firstFrame = false
	} else if frameCnt == c.prevFrameCnt {
		frame.ShutterActive = true
	}

	c.prevFrameCnt = frameCnt
}

// readBulkFrame accumulates bulk USB reads into c.frameBuf until totalSize bytes
// have been received, resyncing on unexpected marker-sized mid-frame reads.
func (c *P3Camera) readBulkFrame(totalSize int) error {
	pos := 0
	chunk := make([]byte, chunkSize)

	for pos < totalSize {
		bytesRead, err := c.ep.Read(chunk)
		if err != nil {
			return fmt.Errorf("bulk read: %w", err)
		}

		nextPos := pos + bytesRead
		if (bytesRead == markerSize && nextPos < totalSize) || (nextPos >= totalSize && bytesRead != markerSize) {
			pos = 0
			log.Println("frame sync: dropping, resetting")

			continue
		}

		copy(c.frameBuf[pos:pos+bytesRead], chunk[:bytesRead])
		pos = nextPos
	}

	return nil
}

func (c *P3Camera) ReadFrame() (*Frame, error) {
	if !c.streaming || c.ep == nil {
		return nil, fmt.Errorf("not streaming")
	}

	totalSize := p3FrameSize + markerCount*markerSize

	if err := c.readBulkFrame(totalSize); err != nil {
		return nil, err
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

	c.updateShutterState(frame)

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
		if err := c.StopStreaming(); err != nil {
			log.Printf("stop streaming on close: %v", err)
		}
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
	if err := c.readStatus(); err != nil {
		return fmt.Errorf("shutter status: %w", err)
	}

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
	if err := c.readStatus(); err != nil {
		return fmt.Errorf("gain status: %w", err)
	}
	c.gainMode = mode

	return nil
}

// --- internal USB helpers ---

func (c *P3Camera) sendCommand(cmd []byte) error {
	_, err := c.dev.Control(
		usbCmdSend, // OUT | VENDOR | INTERFACE
		usbBRequestSend,
		0, 0,
		cmd,
	)
	if err != nil {
		return fmt.Errorf("sendCommand: %w", err)
	}

	return nil
}

func (c *P3Camera) readResponse(length int) ([]byte, error) {
	buf := make([]byte, length)
	readLen, err := c.dev.Control(usbCtrlVendorIn, usbBRequestResponse, 0, 0, buf)
	if err != nil {
		return nil, fmt.Errorf("readResponse: %w", err)
	}

	return buf[:readLen], nil
}

func (c *P3Camera) readStatus() error {
	buf := make([]byte, 1)
	_, err := c.dev.Control(usbCtrlVendorIn, usbBRequestStatus, 0, 0, buf)
	if err != nil {
		return fmt.Errorf("readStatus: %w", err)
	}

	return nil
}

func (c *P3Camera) readRegister(cmdName string, length int) ([]byte, error) {
	if err := c.sendCommand(commands[cmdName]); err != nil {
		return nil, err
	}
	if err := c.readStatus(); err != nil {
		return nil, err
	}
	data, err := c.readResponse(length)
	if err != nil {
		return nil, err
	}
	if err := c.readStatus(); err != nil {
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

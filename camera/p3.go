package camera

import (
	"bytes"
	"encoding/binary"
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
	markerCount = 2  // number of start/end markers surrounding each frame
	markerSize  = 12 // bytes per frame marker

	// P3 sensor dimensions.
	p3SensorW = 256
	p3SensorH = 192

	// uint16ByteSize is the number of bytes per raw pixel sample.
	uint16ByteSize = 2

	// Frame layout: 192 IR rows + 2 metadata rows + 192 thermal rows = 386 total
	p3MetaRows  = 2 // number of metadata rows between IR and thermal data
	p3FrameRows = 2*p3SensorH + p3MetaRows
	// p3FrameSize is the frame data size in bytes (excluding markers).
	p3FrameSize = uint16ByteSize * p3FrameRows * p3SensorW // 197,632

	// Temperature conversion constants for the P3 sensor (1/64 K per raw count).
	p3RawThermalScale = 64.0
	p3CelsiusPerCount = 1.0 / p3RawThermalScale
	p3CelsiusBase     = -kelvinOffset

	// p3MetaCount is the number of uint16 values in the metadata rows.
	p3MetaCount = uint16ByteSize * p3SensorW // 2 rows × 256 cols
)

// frameMarker represents the 12-byte start/end frame marker sent by the P3.
type frameMarker struct {
	Length uint8
	Sync   uint8
	Cnt1   uint32
	Cnt2   uint32
	Cnt3   uint16
}

func parseMarker(data []byte) frameMarker {
	return frameMarker{
		Length: data[0],
		Sync:   data[1],
		Cnt1:   binary.LittleEndian.Uint32(data[2:6]),
		Cnt2:   binary.LittleEndian.Uint32(data[6:10]),
		Cnt3:   binary.LittleEndian.Uint16(data[10:12]),
	}
}

// parseP3Frame parses raw USB frame data (start marker + pixel data) into its
// component planes. Input must be at least markerSize + p3FrameSize bytes.
//
// Returns:
//   - ir:       8-bit hardware-AGC brightness, one value per pixel
//   - thermal:  raw uint16 thermal counts (1/64 K per LSB, absolute kelvin)
//   - metadata: raw uint16 metadata registers (2 rows × 256 columns)
func parseP3Frame(data []byte) (ir []uint8, thermal []uint16, metadata []uint16, err error) {
	expected := markerSize + p3FrameSize
	if len(data) < expected {
		return nil, nil, nil, fmt.Errorf("frame too short: got %d, want %d", len(data), expected)
	}

	// Skip the 12-byte start marker; interpret pixel data as uint16 LE.
	pixels := data[markerSize : markerSize+p3FrameSize]

	pixelCount := p3SensorH * p3SensorW

	// IR brightness: rows 0..191, low byte of each uint16.
	ir = make([]uint8, pixelCount)
	for i := range pixelCount {
		ir[i] = pixels[i*uint16ByteSize]
	}

	// Metadata: rows 192..193 (2 rows × 256 cols × 2 bytes).
	metaOffset := p3SensorH * p3SensorW * uint16ByteSize
	metadata = make([]uint16, p3MetaCount)
	for i := range p3MetaCount {
		off := metaOffset + i*uint16ByteSize
		metadata[i] = binary.LittleEndian.Uint16(pixels[off : off+uint16ByteSize])
	}

	// Thermal data: rows 194..385.
	thermalOffset := (p3SensorH + p3MetaRows) * p3SensorW * uint16ByteSize
	thermal = make([]uint16, pixelCount)
	for i := range pixelCount {
		off := thermalOffset + i*uint16ByteSize
		thermal[i] = binary.LittleEndian.Uint16(pixels[off : off+uint16ByteSize])
	}

	return ir, thermal, metadata, nil
}

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

// updateShutterState sets frame.ShutterActive based on metadata register 64
// (the camera's hardware frame counter). When the counter stops incrementing
// the shutter is closed and NUC calibration is in progress.
func (c *P3Camera) updateShutterState(frame *Frame, metadata []uint16) {
	if len(metadata) <= metadataMaxIdx {
		return
	}

	frameCnt := metadata[metadataMaxIdx]

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

	irPlane, thermal, metadata, err := parseP3Frame(c.frameBuf[:markerSize+p3FrameSize])
	if err != nil {
		return nil, err
	}

	// Convert raw thermal counts (1/64 K per LSB) to degrees Celsius.
	celsius := make([]float32, len(thermal))
	for i, raw := range thermal {
		celsius[i] = float32(raw)*p3CelsiusPerCount + p3CelsiusBase
	}

	frame := &Frame{
		Width:   p3SensorW,
		Height:  p3SensorH,
		IR:      irPlane,
		Celsius: celsius,
	}

	c.updateShutterState(frame, metadata)

	return frame, nil
}

// ReadMetadata reads one raw frame and returns the two metadata rows
// (2 × 256 uint16 values) without computing temperatures. Intended for
// P3-specific debugging and register reverse-engineering.
func (c *P3Camera) ReadMetadata() ([]uint16, error) {
	if !c.streaming || c.ep == nil {
		return nil, fmt.Errorf("not streaming")
	}

	totalSize := p3FrameSize + markerCount*markerSize

	if err := c.readBulkFrame(totalSize); err != nil {
		return nil, err
	}

	_, _, metadata, err := parseP3Frame(c.frameBuf[:markerSize+p3FrameSize])

	return metadata, err
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

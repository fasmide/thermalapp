package camera

import (
	"encoding/binary"
	"fmt"
)

const (
	markerSize = 12

	// P3 sensor dimensions
	p3SensorW = 256
	p3SensorH = 192

	// Frame layout: 192 IR rows + 2 metadata rows + 192 thermal rows = 386 total
	p3FrameRows = 2*p3SensorH + 2
	// Frame data size in bytes (excluding markers)
	p3FrameSize = 2 * p3FrameRows * p3SensorW // 197,632

	// Sync byte values for frame markers
	syncStartEven = 0x8C
	syncStartOdd  = 0x8D
	syncEndEven   = 0x8E
	syncEndOdd    = 0x8F
)

// frameMarker represents the 12-byte start/end frame marker.
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

func (m frameMarker) isStart() bool {
	return m.Sync == syncStartEven || m.Sync == syncStartOdd
}

// ParseP3Frame parses raw USB frame data (start marker + pixel data) into a Frame.
// Input should be markerSize + p3FrameSize bytes.
func ParseP3Frame(data []byte) (*Frame, error) {
	expected := markerSize + p3FrameSize
	if len(data) < expected {
		return nil, fmt.Errorf("frame too short: got %d, want %d", len(data), expected)
	}

	// Skip the 12-byte start marker, interpret pixel data as uint16 LE
	pixels := data[markerSize : markerSize+p3FrameSize]

	frame := &Frame{
		Width:  p3SensorW,
		Height: p3SensorH,
	}

	// Parse IR brightness: rows 0..191, low byte of each uint16
	irCount := p3SensorH * p3SensorW
	frame.IR = make([]uint8, irCount)
	for i := 0; i < irCount; i++ {
		// Each pixel is 2 bytes LE; IR brightness is the low byte
		frame.IR[i] = pixels[i*2]
	}

	// Parse metadata: rows 192..193 (2 rows × 256 cols × 2 bytes)
	metaOffset := p3SensorH * p3SensorW * 2
	metaCount := 2 * p3SensorW
	frame.Metadata = make([]uint16, metaCount)
	for i := 0; i < metaCount; i++ {
		off := metaOffset + i*2
		frame.Metadata[i] = binary.LittleEndian.Uint16(pixels[off : off+2])
	}

	// Parse thermal data: rows 194..385
	thermalOffset := (p3SensorH + 2) * p3SensorW * 2
	thermalCount := p3SensorH * p3SensorW
	frame.Thermal = make([]uint16, thermalCount)
	for i := 0; i < thermalCount; i++ {
		off := thermalOffset + i*2
		frame.Thermal[i] = binary.LittleEndian.Uint16(pixels[off : off+2])
	}

	return frame, nil
}

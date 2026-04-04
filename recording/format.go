// Package recording implements radiometric thermal frame recording and playback.
//
// File format (.tha):
//
//	Header (32 bytes):
//	  [0:8]   Magic "THERMAP\x00"
//	  [8:10]  Version uint16 LE (5)
//	  [10:12] SensorWidth uint16 LE
//	  [12:14] SensorHeight uint16 LE
//	  [14:18] FrameCount uint32 LE (updated on close)
//	  [18:26] StartTime int64 LE (Unix nanoseconds)
//	  [26:32] Reserved (zero)
//
//	Per frame (deflate compressed):
//	  [0:4]              CompressedSize uint32 LE
//	  [4:4+N]            Deflate-compressed block containing:
//	    [0:8]              TimestampNs int64 LE (nanos since recording start)
//	    [8]                Flags uint8 (bit 0 = ShutterActive)
//	    [9:9+W*H*4]        Celsius []float32 LE
//	    [9+W*H*4:9+W*H*5]  IR []uint8
package recording

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

var magic = [8]byte{'T', 'H', 'E', 'R', 'M', 'A', 'P', 0}

const (
	headerSize         = 32
	formatVersion      = 5
	timestampSize      = 8
	frameMetaSize      = 1 // 1 byte flags
	frameSizePrefixLen = 4
	celsiusFloat32Size = 4 // bytes per float32 sample

	// Frame flags (Flags byte, offset 8 within uncompressed payload).
	flagShutterActive uint8 = 0x01 // bit 0: shutter closed during this frame
)

// Header is the file header.
type Header struct {
	Width      uint16
	Height     uint16
	FrameCount uint32
	StartTime  int64 // Unix nanoseconds
}

// frameDataSize returns the byte size of one uncompressed frame payload (excluding timestamp and flags).
func (h Header) frameDataSize() int {
	w, ht := int(h.Width), int(h.Height)
	celsius := w * ht * celsiusFloat32Size
	ir := w * ht

	return celsius + ir
}

// framePayloadSize returns the full uncompressed frame size including timestamp and flags.
func (h Header) framePayloadSize() int {
	return timestampSize + frameMetaSize + h.frameDataSize()
}

// frameMaxPayloadSize returns the same as framePayloadSize — kept for symmetry
// with the recorder/player buffer-sizing API.
func (h Header) frameMaxPayloadSize() int {
	return h.framePayloadSize()
}

func writeHeader(out io.Writer, hdr Header) error {
	var buf [headerSize]byte
	copy(buf[0:8], magic[:])
	binary.LittleEndian.PutUint16(buf[8:10], formatVersion)
	binary.LittleEndian.PutUint16(buf[10:12], hdr.Width)
	binary.LittleEndian.PutUint16(buf[12:14], hdr.Height)
	binary.LittleEndian.PutUint32(buf[14:18], hdr.FrameCount)
	binary.LittleEndian.PutUint64(buf[18:26], uint64(hdr.StartTime))
	_, err := out.Write(buf[:])
	if err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	return nil
}

func readHeader(r io.Reader) (Header, error) {
	var buf [headerSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return Header{}, fmt.Errorf("read header: %w", err)
	}
	if !bytes.Equal(buf[:8], magic[:]) {
		return Header{}, fmt.Errorf("not a .tha file (bad magic)")
	}
	ver := binary.LittleEndian.Uint16(buf[8:10])
	if ver != formatVersion {
		return Header{}, fmt.Errorf("unsupported format version %d (expected %d)", ver, formatVersion)
	}

	return Header{
		Width:      binary.LittleEndian.Uint16(buf[10:12]),
		Height:     binary.LittleEndian.Uint16(buf[12:14]),
		FrameCount: binary.LittleEndian.Uint32(buf[14:18]),
		StartTime:  int64(binary.LittleEndian.Uint64(buf[18:26])),
	}, nil
}

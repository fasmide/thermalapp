// Package recording implements radiometric thermal frame recording and playback.
//
// File format (.tha):
//
//	Header (32 bytes):
//	  [0:8]   Magic "THERMAP\x00"
//	  [8:10]  Version uint16 LE (4)
//	  [10:12] SensorWidth uint16 LE
//	  [12:14] SensorHeight uint16 LE
//	  [14:18] FrameCount uint32 LE (updated on close)
//	  [18:26] StartTime int64 LE (Unix nanoseconds)
//	  [26:32] Reserved (zero)
//
//	Per frame (deflate compressed):
//	  [0:4]   CompressedSize uint32 LE
//	  [4:4+N] Deflate-compressed block containing:
//	          [0:8]                TimestampNs int64 LE (nanos since recording start)
//	          [8]                  Flags uint8 (bit 0 = ShutterActive)
//	          [9:11]              HardwareFrameCounter uint16 LE
//	          [11:11+W*H*2]       Thermal []uint16 LE
//	          [11+W*H*2:11+W*H*3] IR []uint8
package recording

import (
	"encoding/binary"
	"fmt"
	"io"
)

var magic = [8]byte{'T', 'H', 'E', 'R', 'M', 'A', 'P', 0}

const (
	headerSize         = 32
	formatVersion      = 4
	timestampSize      = 8
	frameMetaSize      = 3 // 1 byte flags + 2 bytes HardwareFrameCounter
	frameSizePrefixLen = 4
)

// Header is the file header.
type Header struct {
	Width      uint16
	Height     uint16
	FrameCount uint32
	StartTime  int64 // Unix nanoseconds
}

// frameDataSize returns the byte size of one uncompressed frame payload (excluding timestamp).
func (h Header) frameDataSize() int {
	w, ht := int(h.Width), int(h.Height)
	thermal := w * ht * 2
	ir := w * ht

	return thermal + ir
}

// framePayloadSize returns the uncompressed frame size including timestamp and frame metadata.
func (h Header) framePayloadSize() int {
	return timestampSize + frameMetaSize + h.frameDataSize()
}

func writeHeader(w io.Writer, h Header) error {
	var buf [headerSize]byte
	copy(buf[0:8], magic[:])
	binary.LittleEndian.PutUint16(buf[8:10], formatVersion)
	binary.LittleEndian.PutUint16(buf[10:12], h.Width)
	binary.LittleEndian.PutUint16(buf[12:14], h.Height)
	binary.LittleEndian.PutUint32(buf[14:18], h.FrameCount)
	binary.LittleEndian.PutUint64(buf[18:26], uint64(h.StartTime))
	_, err := w.Write(buf[:])

	return err
}

func readHeader(r io.Reader) (Header, error) {
	var buf [headerSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return Header{}, fmt.Errorf("read header: %w", err)
	}
	if buf[0] != magic[0] || buf[1] != magic[1] || buf[2] != magic[2] ||
		buf[3] != magic[3] || buf[4] != magic[4] || buf[5] != magic[5] ||
		buf[6] != magic[6] || buf[7] != magic[7] {
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

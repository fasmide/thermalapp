package recording

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"sync"
	"time"

	"thermalapp/camera"
)

// Recorder writes raw thermal frames to a .tha file (deflate-compressed).
type Recorder struct {
	mu           sync.Mutex
	file         *os.File
	header       Header
	start        time.Time
	frames       uint32
	bytesWritten int64

	// Reusable buffers to avoid allocations per frame
	rawBuf   []byte       // uncompressed frame payload
	compBuf  bytes.Buffer // compressed output
	deflater *flate.Writer
}

// NewRecorder creates a new recording file. Call Close when done.
func NewRecorder(filename string, sensorW, sensorH int) (*Recorder, error) {
	file, err := os.Create(filename)
	if err != nil {
		return nil, fmt.Errorf("create recording: %w", err)
	}

	now := time.Now()
	hdr := Header{
		Width:     uint16(sensorW),
		Height:    uint16(sensorH),
		StartTime: now.UnixNano(),
	}

	if err := writeHeader(file, hdr); err != nil {
		file.Close()
		os.Remove(filename)

		return nil, err
	}

	rec := &Recorder{
		file:         file,
		header:       hdr,
		start:        now,
		rawBuf:       make([]byte, hdr.frameMaxPayloadSize()),
		bytesWritten: headerSize,
	}

	// flate.BestSpeed (level 1) — fast enough for 25fps, still good compression
	rec.deflater, err = flate.NewWriter(&rec.compBuf, flate.BestSpeed)
	if err != nil {
		file.Close()
		os.Remove(filename)

		return nil, fmt.Errorf("create deflater: %w", err)
	}

	return rec, nil
}

// WriteFrame appends a compressed frame to the recording.
func (r *Recorder) WriteFrame(frame *camera.Frame) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return fmt.Errorf("recorder closed")
	}

	// Build the raw payload into r.rawBuf
	elapsed := time.Since(r.start).Nanoseconds()
	off := 0

	// Timestamp
	binary.LittleEndian.PutUint64(r.rawBuf[off:off+8], uint64(elapsed))
	off += 8

	// Flags byte
	var flags uint8
	if frame.ShutterActive {
		flags |= flagShutterActive
	}

	r.rawBuf[off] = flags
	off++

	// Per-pixel Celsius plane (float32 LE)
	off = appendCelsiusPlane(r.rawBuf, off, frame.Celsius)

	// IR data (uint8)
	copy(r.rawBuf[off:], frame.IR)
	off += len(frame.IR)

	// Compress
	r.compBuf.Reset()
	r.deflater.Reset(&r.compBuf)
	if _, err := r.deflater.Write(r.rawBuf[:off]); err != nil {
		return fmt.Errorf("deflate write: %w", err)
	}
	if err := r.deflater.Close(); err != nil {
		return fmt.Errorf("deflate close: %w", err)
	}

	// Write size prefix + compressed data
	var szBuf [frameSizePrefixLen]byte
	binary.LittleEndian.PutUint32(szBuf[:], uint32(r.compBuf.Len()))
	if _, err := r.file.Write(szBuf[:]); err != nil {
		return fmt.Errorf("write frame size: %w", err)
	}
	if _, err := r.file.Write(r.compBuf.Bytes()); err != nil {
		return fmt.Errorf("write frame data: %w", err)
	}

	r.bytesWritten += int64(frameSizePrefixLen) + int64(r.compBuf.Len())
	r.frames++

	return nil
}

// Close updates the frame count in the header and closes the file.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return nil
	}

	// Seek back to header and update frame count
	r.header.FrameCount = r.frames
	if _, err := r.file.Seek(0, 0); err != nil {
		log.Printf("recording: seek to update header: %v", err)
	} else {
		if err := writeHeader(r.file, r.header); err != nil {
			log.Printf("recording: update header: %v", err)
		}
	}

	err := r.file.Close()
	r.file = nil
	log.Printf("recording closed: %d frames", r.frames)

	if err != nil {
		return fmt.Errorf("close recording: %w", err)
	}

	return nil
}

// Frames returns the number of frames written so far.
func (r *Recorder) Frames() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.frames
}

// FileSize returns the current size of the recording file in bytes.
func (r *Recorder) FileSize() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.bytesWritten
}

// DumpFrame writes a single frame to a .tha file (convenience for D key).
func DumpFrame(filename string, frame *camera.Frame) error {
	rec, err := NewRecorder(filename, frame.Width, frame.Height)
	if err != nil {
		return err
	}
	if err := rec.WriteFrame(frame); err != nil {
		rec.Close()

		return err
	}

	return rec.Close()
}

// appendCelsiusPlane encodes celsius as IEEE-754 float32 little-endian into
// dst starting at off and returns the new offset. Used by WriteFrame to keep
// cyclomatic complexity within bounds.
func appendCelsiusPlane(dst []byte, off int, celsius []float32) int {
	for _, tempC := range celsius {
		binary.LittleEndian.PutUint32(dst[off:off+celsiusFloat32Size], math.Float32bits(tempC))
		off += celsiusFloat32Size
	}

	return off
}

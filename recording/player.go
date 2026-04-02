package recording

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"thermalapp/camera"
)

// Player reads frames from a .tha recording file and implements camera.Camera
// so it can be used in place of a live camera for the UI.
type Player struct {
	mu       sync.Mutex
	file     *os.File
	mmap     []byte // memory-mapped file for lock-free random access
	header   Header
	frameBuf []byte // reusable decompressed frame payload buffer
	frameIdx int    // current frame index (0-based)

	// Frame offset index: file offset of each frame's size prefix
	offsets []int64

	// Playback timing
	paused    bool
	lastRead  time.Time
	lastTsNs  int64 // timestamp of last frame read
	frameTsNs int64 // current frame's offset from recording start (nanos)
	firstRead bool
}

// NewPlayer opens a .tha file for playback.
func NewPlayer(filename string) (*Player, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open recording: %w", err)
	}

	h, err := readHeader(f)
	if err != nil {
		f.Close()

		return nil, err
	}

	p := &Player{
		file:      f,
		header:    h,
		frameBuf:  make([]byte, h.framePayloadSize()),
		firstRead: true,
	}

	if err := p.buildIndex(); err != nil {
		f.Close()

		return nil, fmt.Errorf("build index: %w", err)
	}

	// Memory-map the file for lock-free random frame access
	fi, err := f.Stat()
	if err != nil {
		f.Close()

		return nil, fmt.Errorf("stat recording: %w", err)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		f.Close()

		return nil, fmt.Errorf("mmap recording: %w", err)
	}
	// Tell the kernel: random access pattern, and pages can be reclaimed freely.
	_ = syscall.Madvise(data, syscall.MADV_RANDOM)
	_ = syscall.Madvise(data, syscall.MADV_DONTNEED)
	p.mmap = data

	return p, nil
}

// buildIndex scans through the file to record the byte offset of each frame.
func (p *Player) buildIndex() error {
	offsets := make([]int64, 0, p.header.FrameCount)
	pos := int64(headerSize)
	var szBuf [frameSizePrefixLen]byte

	for {
		if _, err := p.file.Seek(pos, 0); err != nil {
			return err
		}
		_, err := io.ReadFull(p.file, szBuf[:])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		cSize := binary.LittleEndian.Uint32(szBuf[:])
		offsets = append(offsets, pos)
		pos += int64(frameSizePrefixLen) + int64(cSize)
	}

	p.offsets = offsets
	if uint32(len(offsets)) != p.header.FrameCount {
		log.Printf("recording: header says %d frames, found %d", p.header.FrameCount, len(offsets))
		p.header.FrameCount = uint32(len(offsets))
	}

	if _, err := p.file.Seek(headerSize, 0); err != nil {
		return err
	}

	return nil
}

// Header returns the file header info.
func (p *Player) Header() Header {
	return p.header
}

// FrameCount returns the total number of frames in the recording.
func (p *Player) FrameCount() uint32 {
	return p.header.FrameCount
}

// FrameIndex returns the current frame index (0-based).
func (p *Player) FrameIndex() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.frameIdx
}

// FrameTime returns the absolute wall-clock time of the current frame.
func (p *Player) FrameTime() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	return time.Unix(0, p.header.StartTime+p.frameTsNs)
}

// Connect is a no-op for playback (satisfies camera.Camera).
func (p *Player) Connect() error { return nil }

// Init returns device info from the recording header.
func (p *Player) Init() (camera.DeviceInfo, error) {
	return camera.DeviceInfo{
		Model: fmt.Sprintf("Recording (%dx%d, %d frames)",
			p.header.Width, p.header.Height, p.header.FrameCount),
	}, nil
}

// StartStreaming is a no-op for playback.
func (p *Player) StartStreaming() error { return nil }

// StopStreaming is a no-op for playback.
func (p *Player) StopStreaming() error { return nil }

// Close releases the file.
func (p *Player) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mmap != nil {
		if err := syscall.Munmap(p.mmap); err != nil {
			log.Printf("munmap: %v", err)
		}
		p.mmap = nil
	}
	if p.file != nil {
		p.file.Close()
		p.file = nil
	}
}

// ReleaseMmapPages advises the kernel that mmap pages are no longer needed
// and can be reclaimed immediately. The mapping stays valid — pages will be
// faulted back in from disk on next access.
func (p *Player) ReleaseMmapPages() {
	p.mu.Lock()
	m := p.mmap
	p.mu.Unlock()
	if m != nil {
		_ = syscall.Madvise(m, syscall.MADV_DONTNEED)
	}
}

// TriggerShutter is a no-op for playback.
func (p *Player) TriggerShutter() error { return nil }

// SetGain is a no-op for playback.
func (p *Player) SetGain(_ camera.GainMode) error { return nil }

// SensorSize returns the recording's sensor dimensions.
func (p *Player) SensorSize() image.Point {
	return image.Point{X: int(p.header.Width), Y: int(p.header.Height)}
}

// SetPaused pauses or resumes playback.
func (p *Player) SetPaused(paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !paused && p.paused {
		p.firstRead = true
	}
	p.paused = paused
}

// IsPaused returns whether playback is paused.
func (p *Player) IsPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.paused
}

// SeekTo seeks to a specific frame index (0-based) and reads that frame.
// The player is paused after seeking.
func (p *Player) SeekTo(idx int) (*camera.Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.file == nil {
		return nil, fmt.Errorf("player closed")
	}

	total := int(p.header.FrameCount)
	if idx < 0 {
		idx = 0
	}
	if total > 0 && idx >= total {
		idx = total - 1
	}

	if err := p.readFrameAt(idx); err != nil {
		return nil, err
	}

	p.frameIdx = idx + 1
	p.paused = true
	p.firstRead = true

	return p.parseFrameData(), nil
}

// ReadFrame reads the next frame, respecting original timing.
// Loops back to start when the recording ends.
func (p *Player) ReadFrame() (*camera.Frame, error) {
	p.mu.Lock()
	paused := p.paused
	p.mu.Unlock()

	if paused {
		time.Sleep(50 * time.Millisecond)

		return nil, fmt.Errorf("paused")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.file == nil {
		return nil, fmt.Errorf("player closed")
	}

	if err := p.readNextFrame(); err != nil {
		return nil, err
	}

	tsNs := int64(binary.LittleEndian.Uint64(p.frameBuf[0:8]))

	now := time.Now()
	if !p.firstRead {
		deltaNs := tsNs - p.lastTsNs
		if deltaNs > 0 {
			elapsed := now.Sub(p.lastRead)
			wait := time.Duration(deltaNs) - elapsed
			if wait > 0 {
				p.mu.Unlock()
				time.Sleep(wait)
				p.mu.Lock()
			}
		}
	}
	p.firstRead = false
	p.lastTsNs = tsNs
	p.frameTsNs = tsNs
	p.lastRead = time.Now()

	frame := p.parseFrameData()
	p.frameIdx++

	return frame, nil
}

// readNextFrame reads the next sequential compressed frame into p.frameBuf.
func (p *Player) readNextFrame() error {
	var szBuf [frameSizePrefixLen]byte
	_, err := io.ReadFull(p.file, szBuf[:])
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		// Loop back to start
		if _, err := p.file.Seek(headerSize, 0); err != nil {
			return fmt.Errorf("seek to start: %w", err)
		}
		p.frameIdx = 0
		p.firstRead = true
		if _, err := io.ReadFull(p.file, szBuf[:]); err != nil {
			return fmt.Errorf("read size after loop: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read frame size: %w", err)
	}

	cSize := binary.LittleEndian.Uint32(szBuf[:])
	compressed := make([]byte, cSize)
	if _, err := io.ReadFull(p.file, compressed); err != nil {
		return fmt.Errorf("read compressed data: %w", err)
	}

	return p.inflate(compressed)
}

// readFrameAt reads a specific frame by index into p.frameBuf using the offset index.
func (p *Player) readFrameAt(idx int) error {
	if idx >= len(p.offsets) {
		return fmt.Errorf("frame %d out of range (have %d)", idx, len(p.offsets))
	}
	if _, err := p.file.Seek(p.offsets[idx], 0); err != nil {
		return fmt.Errorf("seek to frame %d: %w", idx, err)
	}
	var szBuf [frameSizePrefixLen]byte
	if _, err := io.ReadFull(p.file, szBuf[:]); err != nil {
		return fmt.Errorf("read frame %d size: %w", idx, err)
	}
	cSize := binary.LittleEndian.Uint32(szBuf[:])
	compressed := make([]byte, cSize)
	if _, err := io.ReadFull(p.file, compressed); err != nil {
		return fmt.Errorf("read frame %d data: %w", idx, err)
	}
	if err := p.inflate(compressed); err != nil {
		return fmt.Errorf("decompress frame %d: %w", idx, err)
	}
	p.frameTsNs = int64(binary.LittleEndian.Uint64(p.frameBuf[0:8]))

	return nil
}

// inflate decompresses deflate data into p.frameBuf.
func (p *Player) inflate(compressed []byte) error {
	r := flate.NewReader(bytes.NewReader(compressed))
	defer r.Close()
	n, err := io.ReadFull(r, p.frameBuf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("inflate: read %d bytes: %w", n, err)
	}

	return nil
}

// parseFrameData decodes the decompressed frame buffer into a camera.Frame.
func (p *Player) parseFrameData() *camera.Frame {
	w := int(p.header.Width)
	h := int(p.header.Height)
	off := timestampSize

	// Flags byte
	flags := p.frameBuf[off]
	off++

	// HardwareFrameCounter
	hwFrameCnt := binary.LittleEndian.Uint16(p.frameBuf[off : off+2])
	off += 2

	thermalCount := w * h
	thermal := make([]uint16, thermalCount)
	for i := range thermalCount {
		thermal[i] = binary.LittleEndian.Uint16(p.frameBuf[off : off+2])
		off += 2
	}

	ir := make([]uint8, w*h)
	copy(ir, p.frameBuf[off:off+w*h])

	return &camera.Frame{
		Thermal:              thermal,
		IR:                   ir,
		Width:                w,
		Height:               h,
		ShutterActive:        flags&0x01 != 0,
		HardwareFrameCounter: hwFrameCnt,
	}
}

// ReadFrameAt reads a specific frame by index independently of playback state.
// Uses mmap for zero-copy access to compressed data — fully lock-free.
// Multiple goroutines can call this concurrently.
func (p *Player) ReadFrameAt(idx int) (*camera.Frame, time.Time, error) {
	if idx < 0 || idx >= len(p.offsets) {
		return nil, time.Time{}, fmt.Errorf("frame %d out of range (have %d)", idx, len(p.offsets))
	}

	mmap := p.mmap
	if mmap == nil {
		return nil, time.Time{}, fmt.Errorf("player closed")
	}

	off := p.offsets[idx]
	if off+frameSizePrefixLen > int64(len(mmap)) {
		return nil, time.Time{}, fmt.Errorf("frame %d: offset out of bounds", idx)
	}
	cSize := int64(binary.LittleEndian.Uint32(mmap[off : off+frameSizePrefixLen]))
	dataStart := off + frameSizePrefixLen
	dataEnd := dataStart + cSize
	if dataEnd > int64(len(mmap)) {
		return nil, time.Time{}, fmt.Errorf("frame %d: compressed data out of bounds", idx)
	}
	compressed := mmap[dataStart:dataEnd]

	payloadSize := p.header.framePayloadSize()
	w, h := int(p.header.Width), int(p.header.Height)

	// Decompress (the expensive part — runs without any lock)
	buf := make([]byte, payloadSize)
	r := flate.NewReader(bytes.NewReader(compressed))
	n, err := io.ReadFull(r, buf)
	r.Close()
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, time.Time{}, fmt.Errorf("decompress frame %d: read %d bytes: %w", idx, n, err)
	}

	tsNs := int64(binary.LittleEndian.Uint64(buf[0:8]))
	frameTime := time.Unix(0, p.header.StartTime+tsNs)

	foff := timestampSize
	flags := buf[foff]
	foff++
	hwFC := binary.LittleEndian.Uint16(buf[foff : foff+2])
	foff += 2

	thermal := make([]uint16, w*h)
	for i := range thermal {
		thermal[i] = binary.LittleEndian.Uint16(buf[foff : foff+2])
		foff += 2
	}
	ir := make([]uint8, w*h)
	copy(ir, buf[foff:foff+w*h])

	return &camera.Frame{
		Thermal:              thermal,
		IR:                   ir,
		Width:                w,
		Height:               h,
		ShutterActive:        flags&0x01 != 0,
		HardwareFrameCounter: hwFC,
	}, frameTime, nil
}

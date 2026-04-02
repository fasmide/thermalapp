package ui

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"thermalapp/colorize"
	"thermalapp/recording"
)

// availableMemoryBytes returns the available system memory in bytes.
// Falls back to 0 if detection fails.
func availableMemoryBytes() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return kb * 1024
				}
			}
		}
	}

	return 0
}

// PixelQuerier provides historical temperature data at a pixel position.
// Implemented by FrameBuffer (live) and PlaybackBuffer (recording playback).
type PixelQuerier interface {
	QueryPixel(x, y, n int) []Sample
}

// BufferedFrame is a single celsius frame stored in the ring buffer.
type BufferedFrame struct {
	Time    time.Time
	Celsius []float32
}

// FrameBuffer is a ring buffer of celsius frames with a configurable memory cap.
// It stores the colorized (global-emissivity-corrected) celsius values for each
// frame, allowing historical pixel queries at arbitrary positions.
type FrameBuffer struct {
	mu             sync.RWMutex
	frames         []BufferedFrame
	head           int // next write position
	count          int
	maxFrames      int
	maxBytes       int64
	width          int
	height         int
	sampleInterval time.Duration // minimum interval between stored frames (0 = every frame)
	lastAdd        time.Time     // time of last stored frame
}

// NewFrameBuffer creates a frame buffer sized to hold at most maxBytes of data.
func NewFrameBuffer(width, height int, maxBytes int64) *FrameBuffer {
	fb := &FrameBuffer{maxBytes: maxBytes}
	fb.computeMax(width, height)
	fb.frames = make([]BufferedFrame, fb.maxFrames)

	return fb
}

func (fb *FrameBuffer) computeMax(width, height int) {
	fb.width = width
	fb.height = height
	frameBytes := int64(width*height)*4 + 24 // float32 per pixel + struct overhead
	fb.maxFrames = int(fb.maxBytes / frameBytes)
	if fb.maxFrames < 1 {
		fb.maxFrames = 1
	}
}

// Add appends a celsius frame to the buffer. The celsius slice must have
// len == width*height matching the buffer dimensions.
// Frames arriving faster than the configured sample interval are silently dropped.
func (fb *FrameBuffer) Add(celsius []float32, t time.Time) {
	fb.mu.Lock()
	defer fb.mu.Unlock()

	// Throttle: skip frame if it's too soon since the last stored frame
	if fb.sampleInterval > 0 && !fb.lastAdd.IsZero() && t.Sub(fb.lastAdd) < fb.sampleInterval {
		return
	}

	expected := fb.width * fb.height
	if len(celsius) != expected {
		return
	}

	fb.lastAdd = t

	f := &fb.frames[fb.head]
	f.Time = t
	if len(f.Celsius) != expected {
		f.Celsius = make([]float32, expected)
	}
	copy(f.Celsius, celsius)

	fb.head = (fb.head + 1) % fb.maxFrames
	if fb.count < fb.maxFrames {
		fb.count++
	}
}

// QueryPixel returns up to n temperature samples at pixel (x,y) in chronological
// order. If n <= 0, returns all available samples.
func (fb *FrameBuffer) QueryPixel(x, y, n int) []Sample {
	fb.mu.RLock()
	defer fb.mu.RUnlock()

	if fb.count == 0 || x < 0 || y < 0 || x >= fb.width || y >= fb.height {
		return nil
	}

	avail := fb.count
	if n <= 0 || n > avail {
		n = avail
	}

	pixIdx := y*fb.width + x
	out := make([]Sample, n)
	start := (fb.head - n + fb.maxFrames) % fb.maxFrames

	for i := range n {
		f := &fb.frames[(start+i)%fb.maxFrames]
		out[i] = Sample{Time: f.Time, Temp: f.Celsius[pixIdx]}
	}

	return out
}

// Resize clears the buffer and updates the frame dimensions.
func (fb *FrameBuffer) Resize(width, height int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.computeMax(width, height)
	fb.frames = make([]BufferedFrame, fb.maxFrames)
	fb.head = 0
	fb.count = 0
}

// SetMaxBytes changes the memory cap and clears the buffer.
func (fb *FrameBuffer) SetMaxBytes(maxBytes int64) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.maxBytes = maxBytes
	fb.computeMax(fb.width, fb.height)
	fb.frames = make([]BufferedFrame, fb.maxFrames)
	fb.head = 0
	fb.count = 0
}

// MaxBytes returns the current memory cap.
func (fb *FrameBuffer) MaxBytes() int64 {
	fb.mu.RLock()
	defer fb.mu.RUnlock()

	return fb.maxBytes
}

// SampleInterval returns the minimum interval between stored frames.
func (fb *FrameBuffer) SampleInterval() time.Duration {
	fb.mu.RLock()
	defer fb.mu.RUnlock()

	return fb.sampleInterval
}

// SetSampleInterval changes the minimum interval between stored frames.
// A value of 0 means every frame is stored.
func (fb *FrameBuffer) SetSampleInterval(d time.Duration) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.sampleInterval = d
}

// Dims returns the current buffer frame dimensions.
func (fb *FrameBuffer) Dims() (int, int) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()

	return fb.width, fb.height
}

// Len returns the number of frames in the buffer.
func (fb *FrameBuffer) Len() int {
	fb.mu.RLock()
	defer fb.mu.RUnlock()

	return fb.count
}

// ---------------------------------------------------------------------------
// PlaybackBuffer — sparse cache of celsius frames keyed by recording frame index.
// Used during recording playback so graphs can reference the recording timeline
// without duplicating data into a ring buffer (which breaks on seek).
// Supports background backfill: after a seek, a goroutine reads and colorizes
// the skipped frames so the graph fills in progressively.
// ---------------------------------------------------------------------------

type pbEntry struct {
	celsius []float32
	time    time.Time
}

// PlaybackBuffer caches colorized celsius frames by recording frame index.
// On QueryPixel it returns samples up to the current playback position.
type PlaybackBuffer struct {
	mu         sync.RWMutex
	cache      map[int]*pbEntry
	width      int
	height     int
	maxEntries int
	currentIdx int // current playback frame index
	sampleSkip int // store every Nth frame (0 or 1 = every frame)

	// Backfill state
	cancelBackfill context.CancelFunc
	backfillDone   chan struct{} // closed when backfill goroutine exits
}

// NewPlaybackBuffer creates a playback buffer that caches up to maxBytes
// worth of celsius frames.
func NewPlaybackBuffer(width, height int, totalFrames int, maxBytes int64) *PlaybackBuffer {
	frameBytes := int64(width*height)*4 + 64
	maxEntries := int(maxBytes / frameBytes)
	if maxEntries < 1 {
		maxEntries = 1
	}
	if maxEntries > totalFrames {
		maxEntries = totalFrames
	}

	return &PlaybackBuffer{
		cache:      make(map[int]*pbEntry, maxEntries),
		width:      width,
		height:     height,
		maxEntries: maxEntries,
	}
}

// Add caches the celsius frame at the given recording frame index and
// updates the current playback position. Used by UpdateFrame during playback.
// Respects sampleSkip: only frames aligned to the skip grid are stored.
func (pb *PlaybackBuffer) Add(frameIdx int, celsius []float32, t time.Time) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.currentIdx = frameIdx
	skip := pb.sampleSkip
	if skip > 1 && frameIdx%skip != 0 {
		return // not on the sample grid
	}
	pb.store(frameIdx, celsius, t)
}

// store inserts a frame into the cache. Caller must hold pb.mu.
func (pb *PlaybackBuffer) store(frameIdx int, celsius []float32, t time.Time) {
	expected := pb.width * pb.height
	if len(celsius) != expected {
		return
	}

	// Evict if at capacity and this frame isn't already cached
	if _, ok := pb.cache[frameIdx]; !ok && len(pb.cache) >= pb.maxEntries {
		pb.evict(frameIdx)
	}

	e := pb.cache[frameIdx]
	if e == nil {
		e = &pbEntry{celsius: make([]float32, expected)}
		pb.cache[frameIdx] = e
	}
	copy(e.celsius, celsius)
	e.time = t
}

// evict removes the cached frame farthest from currentIdx, but never the
// frame about to be added (addIdx). Caller must hold pb.mu.
func (pb *PlaybackBuffer) evict(addIdx int) {
	farthestIdx := -1
	farthestDist := -1
	for idx := range pb.cache {
		if idx == addIdx {
			continue
		}
		d := idx - pb.currentIdx
		if d < 0 {
			d = -d
		}
		if d > farthestDist {
			farthestDist = d
			farthestIdx = idx
		}
	}
	if farthestIdx >= 0 {
		delete(pb.cache, farthestIdx)
	}
}

// QueryPixel returns up to n temperature samples at pixel (x,y) from cached frames
// up to and including the current playback position, in frame order.
func (pb *PlaybackBuffer) QueryPixel(x, y, n int) []Sample {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	if len(pb.cache) == 0 || x < 0 || y < 0 || x >= pb.width || y >= pb.height {
		return nil
	}

	pixIdx := y*pb.width + x

	// Collect frame indices <= currentIdx
	indices := make([]int, 0, len(pb.cache))
	for idx := range pb.cache {
		if idx <= pb.currentIdx {
			indices = append(indices, idx)
		}
	}
	sort.Ints(indices)

	if n > 0 && len(indices) > n {
		indices = indices[len(indices)-n:]
	}

	out := make([]Sample, len(indices))
	for i, idx := range indices {
		e := pb.cache[idx]
		out[i] = Sample{Time: e.time, Temp: e.celsius[pixIdx]}
	}

	return out
}

// Dims returns the cached frame dimensions.
func (pb *PlaybackBuffer) Dims() (int, int) {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	return pb.width, pb.height
}

// Resize clears the cache and updates dimensions.
func (pb *PlaybackBuffer) Resize(width, height int) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.width = width
	pb.height = height
	pb.cache = make(map[int]*pbEntry)
}

// SetCurrentIdx updates the playback position without adding data.
func (pb *PlaybackBuffer) SetCurrentIdx(idx int) {
	pb.mu.Lock()
	pb.currentIdx = idx
	pb.mu.Unlock()
}

// Clear drops all cached frames.
func (pb *PlaybackBuffer) Clear() {
	pb.mu.Lock()
	pb.cache = make(map[int]*pbEntry)
	pb.mu.Unlock()
}

// Len returns the number of cached frames.
func (pb *PlaybackBuffer) Len() int {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	return len(pb.cache)
}

// MaxLen returns the maximum number of frames the buffer can hold.
func (pb *PlaybackBuffer) MaxLen() int {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	return pb.maxEntries
}

// SetMaxBytes changes the memory cap and clears the cache.
func (pb *PlaybackBuffer) SetMaxBytes(maxBytes int64) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	frameBytes := int64(pb.width*pb.height)*4 + 64
	pb.maxEntries = int(maxBytes / frameBytes)
	if pb.maxEntries < 1 {
		pb.maxEntries = 1
	}
	pb.cache = make(map[int]*pbEntry, pb.maxEntries)
}

// SetSampleSkip sets the frame skip factor. 0 or 1 means every frame.
// Higher values mean only every Nth frame is stored/backfilled.
func (pb *PlaybackBuffer) SetSampleSkip(n int) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.sampleSkip = n
}

// SampleSkip returns the current skip factor.
func (pb *PlaybackBuffer) SampleSkip() int {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	return pb.sampleSkip
}

// StopBackfill cancels any running background backfill and waits for it to finish.
func (pb *PlaybackBuffer) StopBackfill() {
	pb.mu.Lock()
	cancel := pb.cancelBackfill
	done := pb.backfillDone
	pb.cancelBackfill = nil
	pb.backfillDone = nil
	pb.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done // wait for workers to drain
	}
}

// StartBackfill begins reading frames from the player in the background,
// colorizing them, and caching the celsius data. It reads from upToIdx-1
// down to 0, skipping frames already in the cache. The invalidate callback
// is called periodically so graph windows can redraw with the new data.
func (pb *PlaybackBuffer) StartBackfill(player *recording.Player, upToIdx int, params colorize.Params, rotation int, invalidate func()) {
	pb.StopBackfill()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	pb.mu.Lock()
	pb.cancelBackfill = cancel
	pb.backfillDone = done
	pb.mu.Unlock()

	go func() {
		defer close(done)
		pb.runBackfill(ctx, player, upToIdx, params, rotation, invalidate)
	}()
}

func (pb *PlaybackBuffer) runBackfill(ctx context.Context, player *recording.Player, upToIdx int, params colorize.Params, rotation int, invalidate func()) {
	// Release mmap pages when backfill finishes (normal completion or cancellation)
	defer player.ReleaseMmapPages()

	// Build work list: frame indices to backfill (newest first, skip cached)
	// Respect sampleSkip: only include frames that align with the skip grid
	work := make([]int, 0, upToIdx)
	pb.mu.RLock()
	skip := pb.sampleSkip
	if skip < 1 {
		skip = 1
	}
	for i := upToIdx - 1; i >= 0 && len(work) < pb.maxEntries; i -= skip {
		if _, ok := pb.cache[i]; !ok {
			work = append(work, i)
		}
	}
	pb.mu.RUnlock()

	if len(work) == 0 {
		return
	}

	// Feed work items through a channel to N parallel workers
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	ch := make(chan int, workers*2)
	var loaded atomic.Int64
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				if ctx.Err() != nil {
					continue // drain channel without processing
				}
				frame, frameTime, err := player.ReadFrameAt(idx)
				if err != nil || ctx.Err() != nil {
					continue
				}
				result := colorize.Colorize(frame, params).Rotate(rotation)
				if ctx.Err() != nil {
					continue
				}

				pb.mu.Lock()
				pb.store(idx, result.Celsius, frameTime)
				pb.mu.Unlock()

				n := loaded.Add(1)
				if n%100 == 0 {
					invalidate()
				}
			}
		}()
	}

	// Send work, respecting cancellation and buffer capacity
	for _, idx := range work {
		if ctx.Err() != nil {
			break
		}
		pb.mu.RLock()
		full := len(pb.cache) >= pb.maxEntries
		pb.mu.RUnlock()
		if full {
			break
		}
		ch <- idx
	}
	close(ch)
	wg.Wait()

	if loaded.Load() > 0 {
		invalidate()
	}
}

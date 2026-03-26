package ui

import (
	"image/color"
	"sync"
	"time"
)

const historySize = 10000

// SpotKind identifies the type of measurement spot.
type SpotKind int

const (
	SpotMin    SpotKind = iota // Auto-tracking coldest pixel
	SpotMax                    // Auto-tracking hottest pixel
	SpotCursor                 // Follows the mouse pointer
	SpotUser                   // User-placed fixed point
)

// Sample is a single temperature reading with timestamp.
type Sample struct {
	Time time.Time
	Temp float32
}

// Spot is a measurement point on the thermal image.
type Spot struct {
	mu    sync.Mutex
	Index int
	Kind  SpotKind
	Color color.NRGBA

	// Image pixel coordinates (smoothed for min/max via EMA)
	X, Y float32

	// Whether this spot currently has a valid reading
	Active bool

	// Ring buffer of temperature history
	hist    [historySize]Sample
	head    int // next write position
	count   int // number of valid samples (up to historySize)
	lastAdd time.Time
}

// NewSpot creates a spot with the given index, kind and color.
func NewSpot(index int, kind SpotKind, col color.NRGBA) *Spot {
	return &Spot{
		Index: index,
		Kind:  kind,
		Color: col,
	}
}

// Record adds a temperature sample. Safe for concurrent use.
func (s *Spot) Record(temp float32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.hist[s.head] = Sample{Time: now, Temp: temp}
	s.head = (s.head + 1) % historySize
	if s.count < historySize {
		s.count++
	}
	s.lastAdd = now
}

// History returns up to the last n samples in chronological order.
// If n <= 0 or n > available, returns all available samples.
func (s *Spot) History(n int) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()

	avail := s.count
	if n <= 0 || n > avail {
		n = avail
	}
	if n == 0 {
		return nil
	}

	out := make([]Sample, n)
	start := (s.head - n + historySize) % historySize
	for i := 0; i < n; i++ {
		out[i] = s.hist[(start+i)%historySize]
	}
	return out
}

// Count returns the number of recorded samples.
func (s *Spot) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// LastTemp returns the most recent temperature, or 0 if no samples.
func (s *Spot) LastTemp() float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count == 0 {
		return 0
	}
	idx := (s.head - 1 + historySize) % historySize
	return s.hist[idx].Temp
}

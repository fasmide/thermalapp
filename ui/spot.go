package ui

import (
	"image/color"
	"math"
	"sync"
	"time"

	"thermalapp/colorize"
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

	// Per-spot emissivity override. 0 means "use global" (no override).
	Emissivity    float32
	EmissivityIdx int // index into colorize.EmissivityPresets, -1 = use global

	// Ring buffer of temperature history
	hist    [historySize]Sample
	head    int // next write position
	count   int // number of valid samples (up to historySize)
	lastAdd time.Time
}

// NewSpot creates a spot with the given index, kind and color.
func NewSpot(index int, kind SpotKind, col color.NRGBA) *Spot {
	return &Spot{
		Index:         index,
		Kind:          kind,
		Color:         col,
		EmissivityIdx: -1, // use global
	}
}

// SpotState is a point-in-time snapshot of a spot's mutable fields.
// Used by the UI goroutine to read spot state without holding the lock.
type SpotState struct {
	X, Y          float32
	Active        bool
	Emissivity    float32
	EmissivityIdx int
}

// GetState returns a snapshot of the spot's mutable fields. Safe for concurrent use.
func (s *Spot) GetState() SpotState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SpotState{
		X:             s.X,
		Y:             s.Y,
		Active:        s.Active,
		Emissivity:    s.Emissivity,
		EmissivityIdx: s.EmissivityIdx,
	}
}

// SetPosition updates spot coordinates and active flag. Safe for concurrent use.
func (s *Spot) SetPosition(x, y float32, active bool) {
	s.mu.Lock()
	s.X = x
	s.Y = y
	s.Active = active
	s.mu.Unlock()
}

// SetActive sets the active flag. Safe for concurrent use.
func (s *Spot) SetActive(active bool) {
	s.mu.Lock()
	s.Active = active
	s.mu.Unlock()
}

// UpdateEMA applies exponential moving average to the position. Safe for concurrent use.
func (s *Spot) UpdateEMA(newX, newY, alpha float32) {
	s.mu.Lock()
	s.X += alpha * (newX - s.X)
	s.Y += alpha * (newY - s.Y)
	s.Active = true
	s.mu.Unlock()
}

// SetEmissivity sets per-spot emissivity override. Safe for concurrent use.
func (s *Spot) SetEmissivity(eps float32, idx int) {
	s.mu.Lock()
	s.Emissivity = eps
	s.EmissivityIdx = idx
	s.mu.Unlock()
}

// GetEmissivity returns per-spot emissivity. Safe for concurrent use.
func (s *Spot) GetEmissivity() (float32, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Emissivity, s.EmissivityIdx
}

// GetPosition returns the spot's current coordinates. Safe for concurrent use.
func (s *Spot) GetPosition() (float32, float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.X, s.Y
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

// SpotStats holds computed statistics for a spot's history.
type SpotStats struct {
	Count    int
	Min, Max float32
	Mean     float32
	StdDev   float32
	Current  float32
	Duration time.Duration // time span of recorded data
}

// Stats computes statistics over all recorded samples.
func (s *Spot) Stats() SpotStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := SpotStats{Count: s.count}
	if s.count == 0 {
		return st
	}

	start := (s.head - s.count + historySize) % historySize
	st.Min = s.hist[start].Temp
	st.Max = s.hist[start].Temp
	st.Current = s.hist[(s.head-1+historySize)%historySize].Temp

	var sum float64
	for i := 0; i < s.count; i++ {
		t := s.hist[(start+i)%historySize].Temp
		if t < st.Min {
			st.Min = t
		}
		if t > st.Max {
			st.Max = t
		}
		sum += float64(t)
	}
	st.Mean = float32(sum / float64(s.count))

	// Standard deviation
	var variance float64
	for i := 0; i < s.count; i++ {
		t := float64(s.hist[(start+i)%historySize].Temp)
		d := t - float64(st.Mean)
		variance += d * d
	}
	st.StdDev = float32(math.Sqrt(variance / float64(s.count)))

	// Duration
	first := s.hist[start].Time
	last := s.hist[(s.head-1+historySize)%historySize].Time
	st.Duration = last.Sub(first)

	return st
}

// CorrectedTemp returns the temperature at this spot, applying per-spot
// emissivity correction if Emissivity > 0. The globalTemp is the celsius
// value already corrected for the global emissivity. globalEps and ambientC
// are from the colorize Result. Safe for concurrent use.
func (s *Spot) CorrectedTemp(globalTemp, globalEps, ambientC float32) float32 {
	s.mu.Lock()
	eps := s.Emissivity
	s.mu.Unlock()

	if eps <= 0 || eps == globalEps {
		return globalTemp
	}
	raw := globalTemp
	if globalEps > 0 && globalEps < 1.0 {
		raw = globalTemp*globalEps + (1-globalEps)*ambientC
	}
	return colorize.CorrectEmissivity(raw, ambientC, eps)
}

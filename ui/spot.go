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

	// Most recent temperature reading (label display)
	lastTemp float32

	// Timestamp of last position change (for latency measurement)
	lastMove time.Time

	// Ring buffer of temperature history (used by min/max spots)
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
	s.lastMove = time.Now()
	s.mu.Unlock()
}

// SetActive sets the active flag. Safe for concurrent use.
func (s *Spot) SetActive(active bool) {
	s.mu.Lock()
	s.Active = active
	s.mu.Unlock()
}

// LastMoveTime returns when this spot's position last changed.
func (s *Spot) LastMoveTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastMove
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

// Record adds a temperature sample to the ring buffer and updates lastTemp.
// Used by min/max spots which track moving positions. Safe for concurrent use.
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
	s.lastTemp = temp
}

// SetLastTemp updates the most recent temperature without recording to history.
// Used by cursor/user spots whose graphs are backed by the frame buffer.
func (s *Spot) SetLastTemp(temp float32) {
	s.mu.Lock()
	s.lastTemp = temp
	s.mu.Unlock()
}

// History returns up to the last maxSamples samples in chronological order.
// If maxSamples <= 0 or maxSamples > available, returns all available samples.
func (s *Spot) History(maxSamples int) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()

	avail := s.count
	if maxSamples <= 0 || maxSamples > avail {
		maxSamples = avail
	}
	if maxSamples == 0 {
		return nil
	}

	out := make([]Sample, maxSamples)
	start := (s.head - maxSamples + historySize) % historySize
	for i := range maxSamples {
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

// LastTemp returns the most recent temperature. Safe for concurrent use.
func (s *Spot) LastTemp() float32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastTemp
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

	stats := SpotStats{Count: s.count}
	if s.count == 0 {
		return stats
	}

	start := (s.head - s.count + historySize) % historySize
	stats.Min = s.hist[start].Temp
	stats.Max = s.hist[start].Temp
	stats.Current = s.hist[(s.head-1+historySize)%historySize].Temp

	var sum float64
	for i := range s.count {
		temp := s.hist[(start+i)%historySize].Temp
		if temp < stats.Min {
			stats.Min = temp
		}
		if temp > stats.Max {
			stats.Max = temp
		}
		sum += float64(temp)
	}
	stats.Mean = float32(sum / float64(s.count))

	// Standard deviation
	var variance float64
	for i := range s.count {
		tempF := float64(s.hist[(start+i)%historySize].Temp)
		d := tempF - float64(stats.Mean)
		variance += d * d
	}
	stats.StdDev = float32(math.Sqrt(variance / float64(s.count)))

	// Duration
	first := s.hist[start].Time
	last := s.hist[(s.head-1+historySize)%historySize].Time
	stats.Duration = last.Sub(first)

	return stats
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

// ComputeStats computes statistics over a slice of samples.
func ComputeStats(samples []Sample) SpotStats {
	stats := SpotStats{Count: len(samples)}
	if len(samples) == 0 {
		return stats
	}

	stats.Min = samples[0].Temp
	stats.Max = samples[0].Temp
	stats.Current = samples[len(samples)-1].Temp

	var sum float64
	for _, samp := range samples {
		if samp.Temp < stats.Min {
			stats.Min = samp.Temp
		}
		if samp.Temp > stats.Max {
			stats.Max = samp.Temp
		}
		sum += float64(samp.Temp)
	}
	stats.Mean = float32(sum / float64(len(samples)))

	var variance float64
	for _, samp := range samples {
		d := float64(samp.Temp) - float64(stats.Mean)
		variance += d * d
	}
	stats.StdDev = float32(math.Sqrt(variance / float64(len(samples))))

	stats.Duration = samples[len(samples)-1].Time.Sub(samples[0].Time)

	return stats
}

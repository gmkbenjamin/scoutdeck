// Package motion holds the robot's closed-loop movement logic: obstacle
// limiting from the forward time-of-flight sensor, dead-reckoning pose
// tracking from the gyro and wheel odometry, bounded moves and turns, and
// recorded patrol routes. Nothing here does I/O, so it is tested without a robot.
package motion

import (
	"math"
	"sort"
	"time"

	"github.com/gmkbenjamin/scoutdeck/internal/control"
)

// Obstacle guard distances, in metres from the front time-of-flight sensor.
const (
	GuardStop  = 0.18 // no forward motion closer than this
	GuardSlow  = 0.60 // forward speed ramps down from here to GuardStop
	RangeFresh = 600 * time.Millisecond
)

// Range is one reading from the forward time-of-flight sensor. Meters is +Inf
// when nothing is in range (the sensor reads up to 2 m).
type Range struct {
	Meters float64
	At     time.Time
}

// Fresh reports whether the reading is recent enough to act on.
func (r Range) Fresh(now time.Time) bool {
	return !r.At.IsZero() && now.Sub(r.At) < RangeFresh && !math.IsNaN(r.Meters)
}

// Limit scales forward speed down as an obstacle gets closer and stops it at
// GuardStop. Reversing, strafing and turning are unchanged: the sensor only
// sees ahead. limited reports whether the command was reduced.
func Limit(m control.Motion, r Range, now time.Time) (out control.Motion, limited bool) {
	if m.Y <= 0 || !r.Fresh(now) || math.IsInf(r.Meters, 1) || r.Meters >= GuardSlow {
		return m, false
	}
	f := (r.Meters - GuardStop) / (GuardSlow - GuardStop)
	f = math.Max(0, math.Min(1, f))
	// Keep a crawl so the robot can still approach to the stop distance.
	maxY := math.Max(f*0.3, 0)
	if r.Meters > GuardStop {
		maxY = math.Max(maxY, 0.04)
	}
	if m.Y <= maxY {
		return m, false
	}
	m.Y = maxY
	return m, true
}

// Blocked reports whether forward motion is stopped by an obstacle.
func Blocked(r Range, now time.Time) bool {
	return r.Fresh(now) && r.Meters <= GuardStop
}

// RangeFilter steadies the time-of-flight sensor, which alternates between
// a distance and "nothing in range" when a target is near its limits. It
// keeps the last second of readings and reports an obstacle once at least
// 40% of them see one (clear again below 20%), at the median of those
// distances.
type RangeFilter struct {
	buf      []Range
	obstacle bool
	prev     float64 // previous raw reading
}

const filterWindow = time.Second

// Add stores a reading and returns the filtered range.
func (f *RangeFilter) Add(r Range) Range {
	keep := f.buf[:0]
	for _, old := range f.buf {
		if r.At.Sub(old.At) < filterWindow {
			keep = append(keep, old)
		}
	}
	f.buf = append(keep, r)
	defer func() { f.prev = r.Meters }()
	var hits []float64
	for _, x := range f.buf {
		if !math.IsInf(x.Meters, 1) {
			hits = append(hits, x.Meters)
		}
	}
	share := float64(len(hits)) / float64(len(f.buf))
	switch {
	case share >= 0.4:
		f.obstacle = true
	case share < 0.2:
		f.obstacle = false
	}
	if !f.obstacle || len(hits) == 0 {
		return Range{Meters: math.Inf(1), At: r.At}
	}
	sort.Float64s(hits)
	m := hits[len(hits)/2]
	// Two close readings in a row count at once (about 0.2 s): stopping must
	// not wait for the median, but one stray reflection must not stop a move.
	if close := math.Max(r.Meters, f.prev); !math.IsInf(close, 1) && close < m {
		m = close
	}
	return Range{Meters: m, At: r.At}
}

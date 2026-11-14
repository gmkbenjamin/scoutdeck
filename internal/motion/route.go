package motion

import (
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/gmkbenjamin/scoutdeck/internal/control"
)

// A Route is a recorded drive that can be replayed as a patrol. Recording
// keeps runs of identical commands together with what the sensors measured
// during them; replay turns pure rotations and straight runs into closed-loop
// goals and plays anything else (arcs, diagonals) for the recorded time.
type Route struct {
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	Steps    []Step    `json:"steps"`
	Distance float64   `json:"distance_m"`
	Duration float64   `json:"duration_s"`
	// EveryMinutes runs the patrol on a schedule while the app is connected
	// and the robot is charged (0 = manual only).
	EveryMinutes int       `json:"every_minutes,omitempty"`
	LastRun      time.Time `json:"last_run,omitempty"`
	DockAfter    bool      `json:"dock_after"`
}

// Step is one run of a constant command.
type Step struct {
	Motion   control.Motion `json:"motion"`
	Seconds  float64        `json:"seconds"`
	Distance float64        `json:"distance_m"` // measured along the commanded axis
	Turn     float64        `json:"turn_rad"`   // measured rotation
}

var routeName = regexp.MustCompile(`^[\p{L}\p{N} _.'-]{1,40}$`)

// ValidName checks a route name: letters, numbers, spaces and - _ . '
func ValidName(name string) error {
	if !routeName.MatchString(name) {
		return fmt.Errorf("route names are 1-40 letters, numbers, spaces or - _ . '")
	}
	return nil
}

// Recorder builds a Route from the commands sent while the user drives.
type Recorder struct {
	steps []Step
	cur   *Step
	start Pose
	began time.Time
	at    time.Time
	first time.Time
}

// MaxRouteSteps bounds a recording; at 10 Hz with changing input this is
// well over ten minutes of driving.
const MaxRouteSteps = 2000

// Sample adds the command in force at now, with the pose at that moment.
// Idle time before the first movement is not recorded.
func (r *Recorder) Sample(m control.Motion, p Pose, now time.Time) {
	m = quantize(m)
	if r.cur != nil && (r.cur.Motion == m || sameAxis(r.cur.Motion, m)) {
		// Same single-axis direction at a different speed (ramping, a
		// gamepad stick): one step, remembered at its fastest.
		if math.Abs(m.X)+math.Abs(m.Y)+math.Abs(m.Z) > math.Abs(r.cur.Motion.X)+math.Abs(r.cur.Motion.Y)+math.Abs(r.cur.Motion.Z) {
			r.cur.Motion = m
		}
		return
	}
	r.close(p, now)
	if r.first.IsZero() && m == (control.Motion{}) {
		return
	}
	if r.first.IsZero() {
		r.first = now
	}
	if len(r.steps) >= MaxRouteSteps {
		return
	}
	r.cur = &Step{Motion: m}
	r.start, r.began = p, now
}

func (r *Recorder) close(p Pose, now time.Time) {
	if r.cur == nil {
		return
	}
	s := *r.cur
	s.Seconds = now.Sub(r.began).Seconds()
	s.Turn = p.Turned - r.start.Turned
	dx, dy := p.X-r.start.X, p.Y-r.start.Y
	axis := r.start.Yaw
	if s.Motion.Y == 0 && s.Motion.X != 0 {
		// Strafe steps track +left distance along Yaw+Pi/2 (Motion.X > 0 moves right).
		axis += math.Pi / 2
	}
	s.Distance = dx*math.Cos(axis) + dy*math.Sin(axis)
	if s.Seconds > 0.05 {
		r.steps = append(r.steps, s)
	}
	r.cur = nil
	r.at = now
}

// Finish closes the recording. Trailing idle time is dropped.
func (r *Recorder) Finish(name string, p Pose, now time.Time) (Route, error) {
	r.close(p, now)
	steps := r.steps
	for len(steps) > 0 && steps[len(steps)-1].Motion == (control.Motion{}) {
		steps = steps[:len(steps)-1]
	}
	if len(steps) == 0 {
		return Route{}, fmt.Errorf("nothing was recorded: drive the route, then save it")
	}
	route := Route{Name: name, Created: now, Steps: steps, DockAfter: true}
	for _, s := range steps {
		route.Duration += s.Seconds
		route.Distance += math.Abs(s.Distance)
	}
	return route, nil
}

// Steps reports how many steps have been recorded so far.
func (r *Recorder) Steps() int {
	n := len(r.steps)
	if r.cur != nil {
		n++
	}
	return n
}

// sameAxis reports whether a and b move along the same single axis in the
// same direction (pure forward/back, pure strafe or pure turn).
func sameAxis(a, b control.Motion) bool {
	axis := func(m control.Motion) (int, float64) {
		switch {
		case m.X != 0 && m.Y == 0 && m.Z == 0:
			return 1, math.Copysign(1, m.X)
		case m.Y != 0 && m.X == 0 && m.Z == 0:
			return 2, math.Copysign(1, m.Y)
		case m.Z != 0 && m.X == 0 && m.Y == 0:
			return 3, math.Copysign(1, m.Z)
		}
		return 0, 0
	}
	ka, sa := axis(a)
	kb, sb := axis(b)
	return ka != 0 && ka == kb && sa == sb
}

func quantize(m control.Motion) control.Motion {
	q := func(v float64) float64 { return math.Round(v*100) / 100 }
	return control.Motion{X: q(m.X), Y: q(m.Y), Z: q(m.Z)}
}

// Plan converts a recorded step into what replay should do: a closed-loop
// goal when the step was a pure turn or straight line with a measured
// result, otherwise the raw command for the recorded time.
func (s Step) Plan(odometry bool) (goal *Goal, timed control.Motion, seconds float64) {
	m := s.Motion
	switch {
	case m == (control.Motion{}):
		return nil, m, math.Min(s.Seconds, 10)
	case m.X == 0 && m.Y == 0 && math.Abs(s.Turn) > 2*math.Pi/180:
		return &Goal{Kind: Turn, Target: s.Turn, Speed: clamp(math.Abs(m.Z), MinTurnRate, 1.8)}, m, 0
	case odometry && m.X == 0 && m.Z == 0 && math.Abs(s.Distance) > 0.03:
		return &Goal{Kind: Drive, Target: s.Distance, Speed: clamp(math.Abs(m.Y), MinSpeed, 0.3)}, m, 0
	case odometry && m.Y == 0 && m.Z == 0 && math.Abs(s.Distance) > 0.03:
		return &Goal{Kind: Strafe, Target: s.Distance, Speed: clamp(math.Abs(m.X), MinSpeed, 0.2)}, m, 0
	default:
		return nil, m, s.Seconds
	}
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// DockApproach is where assisted docking hands over to the firmware: this
// far straight out from the dock, facing it, so the dock's QR code fills the
// camera's view.
const DockApproach = 0.6

// The dock's keep-out area for the robot's centre: the dock body sits just
// behind the docked position, widened by half the robot plus a margin.
const (
	keepOutMinX = -0.45
	keepOutMaxX = -0.02
	keepOutY    = 0.30
	sideY       = 0.60 // detour lane beside the dock
)

func inKeepOut(x, y float64) bool {
	return x > keepOutMinX && x < keepOutMaxX && math.Abs(y) < keepOutY
}

// segmentClear reports whether the straight path avoids the keep-out area.
func segmentClear(x0, y0, x1, y1 float64) bool {
	n := int(math.Hypot(x1-x0, y1-y0)/0.02) + 1
	for i := 0; i <= n; i++ {
		f := float64(i) / float64(n)
		if inKeepOut(x0+(x1-x0)*f, y0+(y1-y0)*f) {
			return false
		}
	}
	return true
}

// approachTarget picks the next point to drive to: the approach point if
// the way is clear, otherwise a detour beside the dock.
func approachTarget(p Pose) (x, y float64, final bool) {
	if segmentClear(p.X, p.Y, DockApproach, 0) {
		return DockApproach, 0, true
	}
	side := sideY
	if p.Y < 0 {
		side = -sideY
	}
	for _, w := range [][2]float64{{0.3, side}, {p.X, side}} {
		if segmentClear(p.X, p.Y, w[0], w[1]) {
			return w[0], w[1], false
		}
	}
	return DockApproach, 0, true // nothing better; the obstacle guard still applies
}

// ApproachPlan returns the goals that take the robot from p toward the
// approach point in front of the dock (the tracker origin, where the robot
// sat docked facing +X), ending face-on to the dock. Callers should run only
// the first goal and plan again from the measured pose, so turn and drive
// errors do not pile up and long or detoured trips arrive in pieces. An empty
// plan means the robot is in place.
func ApproachPlan(p Pose) []Goal {
	tx, ty, final := approachTarget(p)
	dx, dy := tx-p.X, ty-p.Y
	dist := math.Hypot(dx, dy)
	var goals []Goal
	heading := p.Yaw
	if dist > 0.08 {
		bearing := math.Atan2(dy, dx)
		if t := wrap(bearing - heading); math.Abs(t) > 3*math.Pi/180 {
			goals = append(goals, Goal{Kind: Turn, Target: t, Speed: 1.0})
		}
		goals = append(goals, Goal{Kind: Drive, Target: math.Min(dist, MaxDistance), Speed: 0.12})
		heading = bearing
	}
	if !final {
		return goals
	}
	if t := wrap(math.Pi - heading); math.Abs(t) > 3*math.Pi/180 {
		goals = append(goals, Goal{Kind: Turn, Target: t, Speed: 1.0})
	}
	return goals
}

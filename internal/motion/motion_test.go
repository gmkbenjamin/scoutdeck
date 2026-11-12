package motion

import (
	"math"
	"testing"
	"time"

	"github.com/shell-company/scout/internal/control"
)

func TestLimit(t *testing.T) {
	now := time.Now()
	fwd := control.Motion{X: 0.2, Z: 0.5}
	if got, lim := Limit(fwd, Range{Meters: math.Inf(1), At: now}, now); lim || got != fwd {
		t.Fatalf("clear path limited: %+v", got)
	}
	if got, lim := Limit(fwd, Range{Meters: 0.1, At: now}, now); !lim || got.X != 0 || got.Z != 0.5 {
		t.Fatalf("close obstacle: %+v %v", got, lim)
	}
	mid, lim := Limit(fwd, Range{Meters: 0.39, At: now}, now)
	if !lim || mid.X <= 0.04 || mid.X >= 0.2 {
		t.Fatalf("approach should slow: %+v", mid)
	}
	if got, lim := Limit(control.Motion{X: -0.2}, Range{Meters: 0.05, At: now}, now); lim || got.X != -0.2 {
		t.Fatal("reversing must not be limited")
	}
	if _, lim := Limit(fwd, Range{Meters: 0.05, At: now.Add(-time.Second)}, now); lim {
		t.Fatal("stale readings must not limit")
	}
}

// sim drives a perfect robot: commanded velocity becomes gyro and odometry.
type sim struct {
	tr  *Tracker
	now time.Time
}

func (s *sim) run(m control.Motion, d time.Duration, odom bool) {
	s.tr.Command(m, s.now)
	for t := time.Duration(0); t < d; t += 5 * time.Millisecond {
		s.now = s.now.Add(5 * time.Millisecond)
		s.tr.Gyro(m.Z, s.now)
		if odom && t%(100*time.Millisecond) == 0 {
			s.tr.Odometry(m.X, m.Y, s.now)
		}
	}
}

func TestTrackerIntegrates(t *testing.T) {
	s := &sim{tr: &Tracker{}, now: time.Unix(1000, 0)}
	s.tr.Reset(s.now)
	s.run(control.Motion{}, time.Second, true)
	s.run(control.Motion{X: 0.1}, 2*time.Second, true)
	p := s.tr.Pose()
	if math.Abs(p.X-0.2) > 0.02 || math.Abs(p.Y) > 0.01 || p.Source != "odometry" {
		t.Fatalf("straight: %+v", p)
	}
	s.run(control.Motion{Z: math.Pi / 2}, time.Second, true)
	if p = s.tr.Pose(); math.Abs(p.Yaw-math.Pi/2) > 0.02 {
		t.Fatalf("turn: %+v", p)
	}
	s.run(control.Motion{X: 0.1}, time.Second, true)
	if p = s.tr.Pose(); math.Abs(p.X-0.2) > 0.03 || math.Abs(p.Y-0.1) > 0.02 {
		t.Fatalf("after turn: %+v", p)
	}
	if len(s.tr.Trail()) < 5 {
		t.Fatal("trail not recorded")
	}
}

func TestGyroBiasIgnoredWhileStill(t *testing.T) {
	tr := &Tracker{}
	now := time.Unix(0, 0)
	for i := 0; i < 4000; i++ {
		now = now.Add(5 * time.Millisecond)
		tr.Gyro(0.004, now)
	}
	if y := tr.Pose().Yaw; math.Abs(y) > 0.001 {
		t.Fatalf("yaw drifted while still: %v", y)
	}
}

func TestMoverTurnsAndDrives(t *testing.T) {
	for _, g := range []Goal{{Kind: Turn, Target: math.Pi}, {Kind: Turn, Target: -math.Pi / 2}, {Kind: Drive, Target: 0.5}, {Kind: Drive, Target: -0.3}, {Kind: Strafe, Target: 0.2}} {
		if err := g.Validate(); err != nil {
			t.Fatal(err)
		}
		s := &sim{tr: &Tracker{}, now: time.Unix(1000, 0)}
		s.tr.Reset(s.now)
		s.run(control.Motion{X: 0.01}, 300*time.Millisecond, true) // prove odometry
		m := NewMover(g, s.tr.Pose(), s.now)
		var done bool
		var cmd control.Motion
		for i := 0; i < 400 && !done; i++ {
			cmd, done = m.Step(s.tr.Pose(), Range{Meters: math.Inf(1), At: s.now}, s.now)
			s.run(cmd, 100*time.Millisecond, true)
		}
		r := m.Result()
		if !r.Completed || math.Abs(r.Achieved-g.Target) > 0.06 {
			t.Fatalf("%+v: %+v", g, r)
		}
	}
}

func TestMoverStopsForObstacle(t *testing.T) {
	g := Goal{Kind: Drive, Target: 1}
	_ = g.Validate()
	now := time.Now()
	m := NewMover(g, Pose{}, now)
	if _, done := m.Step(Pose{}, Range{Meters: 0.1, At: now}, now); !done || m.Result().StoppedBy != "obstacle" {
		t.Fatalf("%+v", m.Result())
	}
}

func TestGoalValidate(t *testing.T) {
	for _, g := range []Goal{{Kind: Drive, Target: 5}, {Kind: Drive}, {Kind: Turn, Target: 20}, {Kind: "fly", Target: 1}, {Kind: Drive, Target: 1, Speed: 0.5}, {Kind: Turn, Target: math.NaN()}} {
		if g.Validate() == nil {
			t.Fatalf("accepted %+v", g)
		}
	}
}

func TestRouteRecordAndPlan(t *testing.T) {
	s := &sim{tr: &Tracker{}, now: time.Unix(1000, 0)}
	s.tr.Reset(s.now)
	var rec Recorder
	step := func(m control.Motion, d time.Duration) {
		for t := time.Duration(0); t < d; t += 100 * time.Millisecond {
			rec.Sample(m, s.tr.Pose(), s.now)
			s.run(m, 100*time.Millisecond, true)
		}
	}
	step(control.Motion{}, time.Second) // idle before start: dropped
	step(control.Motion{X: 0.15}, 2*time.Second)
	step(control.Motion{Z: 1}, 1500*time.Millisecond)
	step(control.Motion{X: 0.1, Z: 0.3}, time.Second)
	step(control.Motion{}, 2*time.Second) // trailing idle: dropped
	r, err := rec.Finish("Kitchen loop", s.tr.Pose(), s.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Steps) != 3 {
		t.Fatalf("steps: %+v", r.Steps)
	}
	if g, _, _ := r.Steps[0].Plan(true); g == nil || g.Kind != Drive || math.Abs(g.Target-0.3) > 0.03 {
		t.Fatalf("drive plan: %+v", g)
	}
	if g, _, _ := r.Steps[1].Plan(true); g == nil || g.Kind != Turn || math.Abs(g.Target-1.5) > 0.05 {
		t.Fatalf("turn plan: %+v", g)
	}
	if g, m, secs := r.Steps[2].Plan(true); g != nil || m.Z != 0.3 || math.Abs(secs-1) > 0.05 {
		t.Fatalf("arc plan: %+v %+v %v", g, m, secs)
	}
	if g, _, secs := r.Steps[0].Plan(false); g != nil || secs < 1.9 {
		t.Fatal("without odometry, straight runs replay by time")
	}
	if ValidName("Kitchen loop") != nil || ValidName("../x") == nil || ValidName("") == nil {
		t.Fatal("name validation")
	}
}

func TestRangeFilterSteadies(t *testing.T) {
	var f RangeFilter
	now := time.Unix(0, 0)
	inf := math.Inf(1)
	var out Range
	// Alternating readings near the limit: one steady obstacle, not flicker.
	for i, v := range []float64{inf, 0.34, inf, 0.28, inf, 0.31, inf, 0.33, inf, 0.30} {
		out = f.Add(Range{Meters: v, At: now.Add(time.Duration(i) * 110 * time.Millisecond)})
	}
	if math.IsInf(out.Meters, 1) || out.Meters > 0.34 {
		t.Fatalf("expected a steady obstacle, got %v", out.Meters)
	}
	// A single stray reading does not raise an obstacle.
	var g RangeFilter
	for i := 0; i < 9; i++ {
		v := inf
		if i == 4 {
			v = 0.2
		}
		out = g.Add(Range{Meters: v, At: now.Add(time.Duration(i) * 110 * time.Millisecond)})
	}
	if !math.IsInf(out.Meters, 1) {
		t.Fatalf("stray reading became an obstacle: %v", out.Meters)
	}
}

// approach runs the re-planning loop the app uses on a perfect robot.
func approach(t *testing.T, s *sim) Pose {
	for i := 0; i < 12; i++ {
		plan := ApproachPlan(s.tr.Pose())
		if len(plan) == 0 {
			return s.tr.Pose()
		}
		m := NewMover(plan[0], s.tr.Pose(), s.now)
		for done := false; !done; {
			var cmd control.Motion
			cmd, done = m.Step(s.tr.Pose(), Range{Meters: math.Inf(1), At: s.now}, s.now)
			s.run(cmd, 100*time.Millisecond, true)
			if p := s.tr.Pose(); inKeepOut(p.X, p.Y) {
				t.Fatalf("drove through the dock at %+v", p)
			}
		}
	}
	t.Fatal("approach did not converge")
	return Pose{}
}

func TestApproachPlan(t *testing.T) {
	for _, start := range [][3]float64{{0.16, -0.2, 171}, {-1, 0, 0}, {-0.6, 0.3, 90}, {0.6, 5, -90}, {6, 0, 45}, {4, -4, 180}, {0.1, 0, 180}, {0.6, 0, 0}} {
		s := &sim{tr: &Tracker{}, now: time.Unix(1000, 0)}
		s.tr.Reset(s.now)
		s.run(control.Motion{X: 0.01}, 300*time.Millisecond, true)
		s.tr.mu.Lock()
		s.tr.pose.X, s.tr.pose.Y, s.tr.pose.Yaw = start[0], start[1], start[2]*math.Pi/180
		s.tr.mu.Unlock()
		p := approach(t, s)
		if math.Hypot(p.X-DockApproach, p.Y) > 0.1 || math.Abs(wrap(p.Yaw-math.Pi)) > 0.08 {
			t.Fatalf("from %v: ended at %+v", start, p)
		}
	}
}

func TestRecorderMergesRamps(t *testing.T) {
	var rec Recorder
	now := time.Unix(0, 0)
	p := Pose{}
	for _, z := range []float64{-0.35, -0.6, -1, -1, -0.8, -0.4} {
		rec.Sample(control.Motion{Z: z}, p, now)
		now = now.Add(100 * time.Millisecond)
	}
	p.Turned = -0.5
	for _, x := range []float64{0.05, 0.1, 0.15, 0.1, 0.04} {
		rec.Sample(control.Motion{X: x}, p, now)
		now = now.Add(100 * time.Millisecond)
	}
	r, err := rec.Finish("ramps", Pose{X: 0.3, Turned: -0.5}, now)
	if err != nil || len(r.Steps) != 2 || r.Steps[0].Motion.Z != -1 || r.Steps[1].Motion.X != 0.15 {
		t.Fatalf("%v %+v", err, r.Steps)
	}
}

func TestRangeFilterIgnoresOneStrayCloseReading(t *testing.T) {
	var f RangeFilter
	now := time.Unix(0, 0)
	var out Range
	for i, v := range []float64{0.9, 0.95, 0.9, 0.1, 0.92} {
		out = f.Add(Range{Meters: v, At: now.Add(time.Duration(i) * 110 * time.Millisecond)})
		if i == 3 && out.Meters < 0.5 {
			t.Fatalf("one stray reading stopped the robot: %v", out.Meters)
		}
	}
	for i, v := range []float64{0.12, 0.11} {
		out = f.Add(Range{Meters: v, At: now.Add(time.Duration(5+i) * 110 * time.Millisecond)})
	}
	if out.Meters > 0.12 {
		t.Fatalf("two close readings must count at once: %v", out.Meters)
	}
}

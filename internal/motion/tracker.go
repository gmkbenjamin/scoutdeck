package motion

import (
	"math"
	"sync"
	"time"

	"github.com/shell-company/scout/internal/control"
)

// Point is a position in metres in the tracker's world frame. The frame's
// origin is where tracking started or was last reset (the dock, after a
// verified docking), with +X ahead of the robot at that moment and +Y to its left.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Pose is the dead-reckoned position and heading (radians, CCW positive).
type Pose struct {
	X        float64   `json:"x"`
	Y        float64   `json:"y"`
	Yaw      float64   `json:"yaw"`
	Odometer float64   `json:"odometer_m"` // total distance travelled since reset
	Turned   float64   `json:"turned_rad"` // total signed rotation since reset (unwrapped)
	Source   string    `json:"source"`     // "odometry" or "commanded"
	At       time.Time `json:"at"`
}

// Tracker integrates the gyro for heading and wheel odometry (or, until
// odometry has proved itself, the commanded velocity) for translation.
type Tracker struct {
	mu sync.Mutex

	pose      Pose
	trail     []Point
	gyroAt    time.Time
	bias      float64 // gyro z bias, learned while stationary
	biasN     int
	still     time.Time // when commanded motion last became zero
	moving    bool
	cmd       control.Motion
	cmdAt     time.Time
	odom      control.Motion // latest body-frame velocity from odometry
	odomAt    time.Time
	odomValid bool // odometry has reported motion while commanded to move
	transAt   time.Time
	external  bool // the firmware is driving (docking): treat as moving
}

// External marks whether the firmware is moving the robot itself, so slow
// rotations are tracked rather than mistaken for gyro bias or noise.
func (t *Tracker) External(on bool) {
	t.mu.Lock()
	t.external = on
	t.mu.Unlock()
}

const (
	trailStep  = 0.05 // metres between trail points
	trailMax   = 4000
	gyroMaxGap = 100 * time.Millisecond
	odomFresh  = 400 * time.Millisecond
)

// Reset moves the origin to the current position and clears the trail.
func (t *Tracker) Reset(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pose = Pose{At: now, Source: t.pose.Source}
	t.trail = []Point{{}}
}

// Command records the velocity sent to the robot.
func (t *Tracker) Command(m control.Motion, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.integrate(now)
	moving := m != (control.Motion{})
	if !moving && t.moving {
		t.still = now
	}
	t.moving, t.cmd, t.cmdAt = moving, m, now
}

// Gyro integrates one IMU angular-velocity sample (rad/s about +Z).
func (t *Tracker) Gyro(z float64, now time.Time) {
	if math.IsNaN(z) || math.IsInf(z, 0) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Learn the bias while the robot has been still for a moment.
	if !t.moving && !t.external && (t.still.IsZero() || now.Sub(t.still) > 700*time.Millisecond) {
		if math.Abs(z) < 0.05 {
			t.biasN = min(t.biasN+1, 2000)
			t.bias += (z - t.bias) / float64(t.biasN)
		}
	}
	if !t.gyroAt.IsZero() {
		dt := now.Sub(t.gyroAt)
		if dt > 0 && dt <= gyroMaxGap {
			w := z - t.bias
			// Ignore noise while the robot is not being driven.
			if !t.moving && !t.external && math.Abs(w) < 0.02 {
				w = 0
			}
			d := w * dt.Seconds()
			t.integrate(now)
			t.pose.Yaw = wrap(t.pose.Yaw + d)
			t.pose.Turned += d
		}
	}
	t.gyroAt = now
}

// Odometry records the body-frame velocity reported by the wheel odometry.
func (t *Tracker) Odometry(vx, vy float64, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.integrate(now)
	t.odom, t.odomAt = control.Motion{X: vx, Y: vy}, now
	if t.moving && (math.Abs(vx) > 0.01 || math.Abs(vy) > 0.01) {
		t.odomValid = true
	}
}

// integrate advances the position to now using the current velocity source.
// Callers hold t.mu.
func (t *Tracker) integrate(now time.Time) {
	if t.transAt.IsZero() {
		t.transAt = now
		if len(t.trail) == 0 {
			t.trail = []Point{{}}
		}
		return
	}
	dt := now.Sub(t.transAt).Seconds()
	t.transAt = now
	if dt <= 0 || dt > 0.5 {
		return
	}
	v := t.cmd
	t.pose.Source = "commanded"
	if t.odomValid && now.Sub(t.odomAt) < odomFresh {
		v, t.pose.Source = t.odom, "odometry"
	} else if !t.moving {
		v = control.Motion{}
	}
	if v.X == 0 && v.Y == 0 {
		return
	}
	c, s := math.Cos(t.pose.Yaw), math.Sin(t.pose.Yaw)
	dx, dy := (v.X*c-v.Y*s)*dt, (v.X*s+v.Y*c)*dt
	t.pose.X += dx
	t.pose.Y += dy
	t.pose.Odometer += math.Hypot(dx, dy)
	t.pose.At = now
	last := t.trail[len(t.trail)-1]
	if math.Hypot(t.pose.X-last.X, t.pose.Y-last.Y) >= trailStep {
		t.trail = append(t.trail, Point{t.pose.X, t.pose.Y})
		if len(t.trail) > trailMax {
			t.trail = append([]Point(nil), t.trail[len(t.trail)-trailMax/2:]...)
		}
	}
}

// Pose returns the current estimate.
func (t *Tracker) Pose() Pose {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pose
}

// Trail returns a copy of the breadcrumb trail, oldest first.
func (t *Tracker) Trail() []Point {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Point(nil), t.trail...)
}

// OdometryValid reports whether wheel odometry has been seen tracking motion.
func (t *Tracker) OdometryValid() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.odomValid
}

func wrap(a float64) float64 {
	for a > math.Pi {
		a -= 2 * math.Pi
	}
	for a <= -math.Pi {
		a += 2 * math.Pi
	}
	return a
}

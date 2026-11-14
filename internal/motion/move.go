package motion

import (
	"fmt"
	"math"
	"time"

	"github.com/shell-company/scout/internal/control"
)

// Goal kinds.
const (
	Drive  = "drive"  // Target metres along the robot's heading (negative reverses)
	Strafe = "strafe" // Target metres sideways (positive is left)
	Turn   = "turn"   // Target radians (positive is left / counter-clockwise)
)

// Limits for one bounded move.
const (
	MaxDistance = 3.0         // metres
	MaxTurn     = 4 * math.Pi // two full turns
	MinSpeed    = 0.04        // m/s, below this the wheels may stall
	MinTurnRate = 0.35        // rad/s
)

// Goal is one bounded, closed-loop movement.
type Goal struct {
	Kind   string  `json:"kind"`
	Target float64 `json:"target"` // metres or radians, signed
	Speed  float64 `json:"speed"`  // m/s or rad/s, positive; 0 picks a default
}

// Validate checks a goal against the API limits and fills in the default speed.
func (g *Goal) Validate() error {
	if math.IsNaN(g.Target) || math.IsInf(g.Target, 0) || math.IsNaN(g.Speed) || math.IsInf(g.Speed, 0) {
		return fmt.Errorf("target and speed must be finite")
	}
	switch g.Kind {
	case Drive, Strafe:
		if g.Target == 0 || math.Abs(g.Target) > MaxDistance {
			return fmt.Errorf("distance must be non-zero and at most %.1f m", MaxDistance)
		}
		limit := 0.3
		if g.Kind == Strafe {
			limit = 0.2
		}
		if g.Speed == 0 {
			g.Speed = 0.15
		}
		if g.Speed < MinSpeed || g.Speed > limit {
			return fmt.Errorf("speed must be %.2f..%.2f m/s", MinSpeed, limit)
		}
	case Turn:
		if g.Target == 0 || math.Abs(g.Target) > MaxTurn {
			return fmt.Errorf("angle must be non-zero and at most 720 degrees")
		}
		if g.Speed == 0 {
			g.Speed = 1.0
		}
		if g.Speed < MinTurnRate || g.Speed > 1.8 {
			return fmt.Errorf("turn rate must be %.2f..1.8 rad/s", MinTurnRate)
		}
	default:
		return fmt.Errorf("unknown move kind %q", g.Kind)
	}
	return nil
}

// Timeout bounds how long a goal may run: generous for slow starts and
// approaches, but short enough that a stuck robot gives up.
func (g Goal) Timeout() time.Duration {
	return time.Duration((math.Abs(g.Target)/g.Speed*2+3)*float64(time.Second)) + time.Second
}

// Result reports how a move ended.
type Result struct {
	Kind      string  `json:"kind"`
	Target    float64 `json:"target"`
	Achieved  float64 `json:"achieved"`
	Completed bool    `json:"completed"`
	StoppedBy string  `json:"stopped_by,omitempty"` // obstacle, timeout, stalled, manual, cancelled, error
	Error     string  `json:"error,omitempty"`
	Feedback  string  `json:"feedback"` // gyro, odometry or commanded
	Seconds   float64 `json:"seconds"`
}

// Mover steps one goal. Feed it the latest pose each control tick; it returns
// the velocity to command and whether the goal has ended.
type Mover struct {
	Goal    Goal
	start   Pose
	started time.Time
	last    float64
	lastAt  time.Time
	result  Result
}

// NewMover starts a goal at the given pose.
func NewMover(g Goal, at Pose, now time.Time) *Mover {
	return &Mover{Goal: g, start: at, started: now, lastAt: now, result: Result{Kind: g.Kind, Target: g.Target}}
}

// Progress is the signed distance or angle achieved so far.
func (m *Mover) Progress(p Pose) float64 {
	switch m.Goal.Kind {
	case Turn:
		return p.Turned - m.start.Turned
	default:
		dx, dy := p.X-m.start.X, p.Y-m.start.Y
		axis := m.start.Yaw
		if m.Goal.Kind == Strafe {
			axis += math.Pi / 2
		}
		return dx*math.Cos(axis) + dy*math.Sin(axis)
	}
}

// Step returns the next command. done is true once the goal is reached or
// abandoned; Result then explains why.
func (m *Mover) Step(p Pose, r Range, now time.Time) (cmd control.Motion, done bool) {
	g := m.Goal
	got := m.Progress(p)
	m.result.Achieved = got
	m.result.Seconds = now.Sub(m.started).Seconds()
	m.result.Feedback = p.Source
	if g.Kind == Turn {
		m.result.Feedback = "gyro"
	}
	remaining := g.Target - got
	sign := math.Copysign(1, g.Target)
	tolerance := 0.02 // metres
	if g.Kind == Turn {
		tolerance = 2 * math.Pi / 180
	}
	if remaining*sign <= tolerance {
		m.result.Completed = true
		return control.Motion{}, true
	}
	if now.Sub(m.started) > g.Timeout() {
		m.result.StoppedBy = "timeout"
		return control.Motion{}, true
	}
	if g.Kind == Drive && g.Target > 0 && Blocked(r, now) {
		m.result.StoppedBy = "obstacle"
		return control.Motion{}, true
	}
	// Stall: no progress for 2.5 s while commanding motion with real feedback.
	if math.Abs(got-m.last) > tolerance/2 {
		m.last, m.lastAt = got, now
	} else if (g.Kind == Turn || p.Source == "odometry") && now.Sub(m.lastAt) > 2500*time.Millisecond {
		m.result.StoppedBy = "stalled"
		return control.Motion{}, true
	}
	// Proportional approach with a floor so the wheels keep turning.
	gain, floor := 1.2, MinSpeed
	if g.Kind == Turn {
		gain, floor = 2.0, MinTurnRate
	}
	speed := math.Min(g.Speed, math.Max(floor, math.Abs(remaining)*gain))
	// Ease in over the first half second.
	if ramp := now.Sub(m.started).Seconds() / 0.5; ramp < 1 {
		speed = math.Max(floor, speed*ramp)
	}
	v := speed * sign
	switch g.Kind {
	case Drive:
		cmd.Y = v
	case Strafe:
		cmd.X = -v
	case Turn:
		cmd.Z = v
	}
	if g.Kind != Turn {
		// Heading hold: the mecanum wheels drift; steer back to the start heading.
		cmd.Z = math.Max(-0.6, math.Min(0.6, -2.5*wrap(p.Yaw-m.start.Yaw)))
	}
	return cmd, false
}

// Stop ends the goal early for an external reason.
func (m *Mover) Stop(reason string, err error) Result {
	m.result.StoppedBy = reason
	if err != nil {
		m.result.Error = err.Error()
	}
	return m.result
}

// Result returns the outcome so far.
func (m *Mover) Result() Result { return m.result }

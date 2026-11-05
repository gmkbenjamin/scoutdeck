// Package control arbitrates manual and leased agent commands independently of UI and I/O.
package control

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

const Interval = 100 * time.Millisecond
const MaxLease = 500 * time.Millisecond
const ManualTimeout = 350 * time.Millisecond

type Motion struct {
	X float64 `json:"linear_x"`
	Y float64 `json:"linear_y"`
	Z float64 `json:"angular_z"`
}

type Snapshot struct {
	Motion Motion `json:"motion"`
	Owner  string `json:"owner"`
	Error  string `json:"error,omitempty"`
}

type Controller struct {
	mu                    sync.Mutex
	manual, agent         Motion
	manualAt, expires     time.Time
	halt, release, closed bool
	last                  Snapshot
}

func (c *Controller) Manual(m Motion, stop bool, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if stop {
		c.release = true
	}
	if c.release && m == (Motion{}) && !stop {
		c.release = false
	}
	c.halt = stop || c.release
	c.manual, c.manualAt = m, now
	if c.halt || m != (Motion{}) {
		c.expires = time.Time{}
	}
}

func (c *Controller) Drive(m Motion, ttl time.Duration, now time.Time) error {
	for _, v := range []float64{m.X, m.Y, m.Z} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("velocity must be finite")
		}
	}
	if math.Abs(m.X) > .2 || math.Abs(m.Y) > .3 || math.Abs(m.Z) > 1.8 {
		return fmt.Errorf("velocity limits: |linear_x| <= 0.2, |linear_y| <= 0.3, |angular_z| <= 1.8")
	}
	if ttl < Interval || ttl > MaxLease {
		return fmt.Errorf("ttl_ms must be 100..500")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("controller is shutting down")
	}
	if now.Sub(c.manualAt) < ManualTimeout && (c.halt || c.release || c.manual != (Motion{})) {
		return fmt.Errorf("manual controls have priority; release them first")
	}
	c.agent, c.expires = m, now.Add(ttl)
	return nil
}

func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agent, c.expires = Motion{}, time.Time{}
	// Repeated stop requests must not clear the requirement to release held input.
	c.release = c.release || c.manual != (Motion{})
	c.manual = Motion{}
}

// CheckAutonomy preserves manual priority when handing movement to firmware.
func (c *Controller) CheckAutonomy(now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("controller is shutting down")
	}
	if now.Sub(c.manualAt) < ManualTimeout && (c.halt || c.release || c.manual != (Motion{})) {
		return fmt.Errorf("manual controls have priority; release them before docking")
	}
	return nil
}

func (c *Controller) command(now time.Time) Snapshot {
	if !c.closed && now.Sub(c.manualAt) < ManualTimeout {
		if c.halt {
			return Snapshot{Owner: "manual-stop"}
		}
		if c.manual != (Motion{}) {
			return Snapshot{Motion: c.manual, Owner: "manual"}
		}
	}
	if !c.closed && now.Before(c.expires) {
		return Snapshot{Motion: c.agent, Owner: "agent"}
	}
	return Snapshot{Owner: "stopped"}
}

func (c *Controller) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Run owns all velocity writes. Shutdown sends zero before returning to the caller
// that closes the publisher. publish must finish its transport write before returning.
func (c *Controller) Run(ctx context.Context, ticks <-chan time.Time, publish func(Motion) error) error {
	defer func() {
		c.mu.Lock()
		c.closed = true
		c.manual, c.agent, c.expires = Motion{}, Motion{}, time.Time{}
		c.last = Snapshot{Owner: "stopped"}
		c.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return publish(Motion{})
		case <-ticks:
			c.mu.Lock()
			s := c.command(time.Now())
			c.mu.Unlock()
			// Recheck cancellation before submitting a sampled movement command.
			if ctx.Err() != nil {
				return publish(Motion{})
			}
			err := publish(s.Motion)
			if err != nil {
				s.Error = err.Error()
			}
			c.mu.Lock()
			c.last = s
			c.mu.Unlock()
			if err != nil {
				_ = publish(Motion{})
				return err
			}
		}
	}
}

// JoystickMotion deliberately bypasses speed clamping when stop is held.
func JoystickMotion(leftX, leftY, rightX, speed float64, stop bool) Motion {
	if stop {
		return Motion{}
	}
	speed = math.Max(.1, math.Min(1, speed))
	return Motion{X: rightX * .2, Y: leftY * speed, Z: leftX * -2.9}
}

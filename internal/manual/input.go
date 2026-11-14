// Package manual maps desktop keyboard and standard gamepad input without I/O.
package manual

import (
	"fmt"
	"math"

	"github.com/gmkbenjamin/scoutdeck/internal/control"
)

const DeadZone = .15

type Buttons uint16

const (
	Stop Buttons = 1 << iota
	Faster
	Slower
	Dock
	Screenshot
	Dim
	Bright
	AutoLight
	Exit
)

type Keyboard struct {
	Forward, Back, Left, Right, StrafeLeft, StrafeRight bool
	Buttons                                             Buttons
}
type Pad struct {
	ID                   int
	Name                 string
	Standard             bool
	LeftX, LeftY, RightX float64 // Native axes: stick up is negative Y.
	Buttons              Buttons
}
type Frame struct {
	Focused  bool
	Keyboard Keyboard
	Pads     []Pad
}
type Result struct {
	Motion     control.Motion
	Stop, Quit bool
	Actions    []string
	Speed      float64
	Source     string
}
type Mapper struct {
	KeyboardOnly                           bool
	speed                                  float64
	selected                               int
	hasPad, padReady, initialized, blocked bool
	previous                               Buttons
}

func Axis(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) <= DeadZone {
		return 0
	}
	return math.Copysign((math.Min(1, math.Abs(v))-DeadZone)/(1-DeadZone), v)
}

func (m *Mapper) Update(f Frame) Result {
	if !m.initialized {
		m.speed = .2
		m.blocked = true
		m.initialized = true
	}
	r := Result{Speed: m.speed, Source: "Keyboard"}
	var pad *Pad
	if !m.KeyboardOnly {
		for i := range f.Pads {
			p := &f.Pads[i]
			if p.Standard && m.hasPad && p.ID == m.selected {
				pad = p
				break
			}
		}
		if pad == nil {
			for i := range f.Pads {
				if f.Pads[i].Standard {
					pad = &f.Pads[i]
					break
				}
			}
		}
	}
	changed := m.hasPad && (pad == nil || pad.ID != m.selected)
	if changed {
		m.blocked = true
		m.padReady = false
	}
	if pad != nil {
		if !m.hasPad || pad.ID != m.selected {
			m.padReady = false
		}
		m.hasPad = true
		m.selected = pad.ID
		if Axis(pad.LeftX) == 0 && Axis(pad.LeftY) == 0 && Axis(pad.RightX) == 0 && pad.Buttons == 0 {
			m.padReady = true
		}
		r.Source = fmt.Sprintf("Keyboard + %s [controller %d]", pad.Name, pad.ID)
		if !m.padReady {
			r.Source += " - center sticks and release buttons"
		}
	} else {
		m.hasPad = false
		m.padReady = false
		if !m.KeyboardOnly {
			r.Source = "Keyboard - connect an Xbox/standard controller"
			if len(f.Pads) > 0 {
				r.Source = "Keyboard - controller has no standard mapping"
			}
		}
	}
	buttons := f.Keyboard.Buttons
	if pad != nil {
		buttons |= pad.Buttons
	}
	pressed := buttons &^ m.previous
	m.previous = buttons
	if !f.Focused {
		m.blocked = true
		m.padReady = false
		r.Stop = true
		r.Source = "Paused - focus the window, then release controls"
		return r
	}
	if pressed&Exit != 0 {
		r.Quit = true
		r.Stop = true
		return r
	}
	if buttons&Stop != 0 || changed {
		m.blocked = true
	}
	k := f.Keyboard
	keyboardHeld := k.Forward || k.Back || k.Left || k.Right || k.StrafeLeft || k.StrafeRight
	padHeld := pad != nil && (Axis(pad.LeftX) != 0 || Axis(pad.LeftY) != 0 || Axis(pad.RightX) != 0 || pad.Buttons != 0)
	if m.blocked {
		if !keyboardHeld && !padHeld && buttons == 0 && !changed {
			m.blocked = false
		} else {
			r.Stop = true
			return r
		}
	}
	// Buttons held while connecting must not fire once the controller becomes ready.
	if pad != nil && !m.padReady {
		pressed = f.Keyboard.Buttons & pressed
	}
	if pressed&Faster != 0 && buttons&Slower == 0 {
		m.speed = math.Min(.3, m.speed+.1)
	}
	if pressed&Slower != 0 && buttons&Faster == 0 {
		m.speed = math.Max(.1, m.speed-.1)
	}
	r.Speed = m.speed
	if keyboardHeld {
		if k.Forward {
			r.Motion.Y += m.speed
		}
		if k.Back {
			r.Motion.Y -= m.speed
		}
		if k.Left {
			r.Motion.Z += 1.8
		}
		if k.Right {
			r.Motion.Z -= 1.8
		}
		if k.StrafeLeft {
			r.Motion.X -= .2
		}
		if k.StrafeRight {
			r.Motion.X += .2
		}
	} else if pad != nil && m.padReady {
		r.Motion = control.Motion{X: Axis(pad.RightX) * .2, Y: -Axis(pad.LeftY) * m.speed, Z: -Axis(pad.LeftX) * 1.8}
	}
	for _, a := range []struct {
		button Buttons
		name   string
	}{{Dock, "home"}, {Screenshot, "screenshot"}, {Dim, "dim"}, {Bright, "bright"}, {AutoLight, "auto-light"}} {
		if pressed&a.button != 0 {
			r.Actions = append(r.Actions, a.name)
		}
	}
	return r
}

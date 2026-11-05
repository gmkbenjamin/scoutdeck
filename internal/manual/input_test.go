package manual

import (
	"math"
	"testing"

	"github.com/shell-company/scout/internal/control"
)

func ready(pads ...Pad) (*Mapper, Frame) {
	m := &Mapper{}
	f := Frame{Focused: true, Pads: pads}
	m.Update(f)
	return m, f
}
func TestKeyboardAndGamepadFallback(t *testing.T) {
	m, f := ready(Pad{ID: 0, Standard: false}, Pad{ID: 1, Name: "Xbox", Standard: true})
	f.Pads[1].LeftY = -1
	if r := m.Update(f); r.Motion.Y != .2 {
		t.Fatal(r)
	}
	f.Keyboard.Back = true
	if r := m.Update(f); r.Motion.Y != -.2 {
		t.Fatal("keyboard did not override pad", r)
	}
	f.Pads = nil
	if r := m.Update(f); !r.Stop || r.Motion != (control.Motion{}) {
		t.Fatal("disconnect did not stop", r)
	}
	if r := m.Update(f); !r.Stop {
		t.Fatal("held key resumed after disconnect", r)
	}
	f.Keyboard = Keyboard{}
	m.Update(f)
	f.Keyboard.Forward = true
	if r := m.Update(f); r.Motion.Y != .2 {
		t.Fatal("keyboard fallback unavailable", r)
	}
}
func TestDriftClampAndDirection(t *testing.T) {
	for _, v := range []float64{-.15, -.01, 0, .01, .15, math.NaN(), math.Inf(1)} {
		if Axis(v) != 0 {
			t.Fatal(v)
		}
	}
	if Axis(-2) != -1 || Axis(2) != 1 {
		t.Fatal("axes not bounded")
	}
	m, f := ready(Pad{ID: 7, Standard: true})
	f.Pads[0].LeftX = 1
	f.Pads[0].LeftY = -1
	f.Pads[0].RightX = -1
	r := m.Update(f)
	if r.Motion != (control.Motion{X: -.2, Y: .2, Z: -1.8}) {
		t.Fatal(r)
	}
}
func TestHeldControlsOnConnectAndRefocus(t *testing.T) {
	m, f := ready()
	f.Pads = []Pad{{ID: 1, Standard: true, LeftY: -1, Buttons: Dock}}
	if r := m.Update(f); r.Motion != (control.Motion{}) || len(r.Actions) != 0 {
		t.Fatal("held connection input fired", r)
	}
	f.Pads[0].LeftY = 0
	f.Pads[0].Buttons = 0
	m.Update(f)
	f.Pads[0].LeftY = -1
	if r := m.Update(f); r.Motion.Y == 0 {
		t.Fatal(r)
	}
	f.Focused = false
	if r := m.Update(f); !r.Stop {
		t.Fatal(r)
	}
	f.Focused = true
	if r := m.Update(f); !r.Stop {
		t.Fatal("held stick moved after refocus", r)
	}
	f.Pads[0].LeftY = 0
	m.Update(f)
	f.Pads[0].LeftY = -1
	if r := m.Update(f); r.Motion.Y == 0 {
		t.Fatal("neutral did not rearm", r)
	}
}
func TestStopSuppressesMovementAndActionsUntilNeutral(t *testing.T) {
	for _, stop := range []Buttons{Stop, Stop | Faster | Slower} {
		m, f := ready(Pad{ID: 1, Standard: true})
		f.Pads[0].LeftY = -1
		f.Pads[0].Buttons = stop | Dock
		if r := m.Update(f); !r.Stop || r.Motion != (control.Motion{}) || len(r.Actions) > 0 {
			t.Fatal(r)
		}
		f.Pads[0].Buttons = 0
		if r := m.Update(f); !r.Stop {
			t.Fatal("held stick resumed", r)
		}
		f.Pads[0].LeftY = 0
		m.Update(f)
		f.Pads[0].LeftY = -1
		if r := m.Update(f); r.Motion.Y == 0 {
			t.Fatal(r)
		}
	}
}
func TestActionsAndSpeedAreEdges(t *testing.T) {
	m, f := ready(Pad{ID: 1, Standard: true})
	f.Pads[0].Buttons = Screenshot | Dock | Faster
	r := m.Update(f)
	if len(r.Actions) != 2 || r.Speed != .3 {
		t.Fatal(r)
	}
	if r = m.Update(f); len(r.Actions) != 0 || r.Speed != .3 {
		t.Fatal("buttons repeated", r)
	}
	for i := 0; i < 20; i++ {
		f.Pads[0].Buttons = 0
		m.Update(f)
		f.Pads[0].Buttons = Faster
		r = m.Update(f)
	}
	if r.Speed != .3 {
		t.Fatal("speed exceeded limit", r)
	}
}
func TestStableSelectionAndKeyboardOnly(t *testing.T) {
	m, f := ready(Pad{ID: 5, Standard: true})
	f.Pads = []Pad{{ID: 1, Standard: true, LeftY: -1}, {ID: 5, Standard: true}}
	if r := m.Update(f); r.Motion != (control.Motion{}) {
		t.Fatal("new pad stole control", r)
	}
	m.KeyboardOnly = true
	m.Update(f)
	m.Update(f)
	f.Keyboard.Forward = true
	if r := m.Update(f); r.Motion.Y != .2 {
		t.Fatal(r)
	}
}

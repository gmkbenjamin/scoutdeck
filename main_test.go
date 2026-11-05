package main

import (
	"github.com/shell-company/scout/internal/control"
	"github.com/shell-company/scout/internal/manual"
	"sync"
	"testing"
	"time"
)

func TestBatterySnapshotConcurrent(t *testing.T) {
	a := &application{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				onBatteryStatus(&status{Status: []int32{1, 75}})
				_ = a.status()
			}
		}()
	}
	wg.Wait()
	batteryMu.RLock()
	defer batteryMu.RUnlock()
	if batteryPercent != 75 || batteryChargeState != 1 || batteryAt.IsZero() {
		t.Fatal("invalid battery snapshot")
	}
}

func TestUnfocusedWindowDoesNotPermanentlyBlockAgents(t *testing.T) {
	g := &Game{app: &application{}}
	g.applyInput(manual.Frame{Focused: true})
	g.applyInput(manual.Frame{Focused: false})
	if err := g.app.control.Drive(control.Motion{Y: .1}, 300*time.Millisecond, time.Now()); err == nil {
		t.Fatal("focus loss did not revoke input")
	}
	g.applyInput(manual.Frame{Focused: false})
	if err := g.app.control.Drive(control.Motion{Y: .1}, 300*time.Millisecond, time.Now()); err != nil {
		t.Fatal("unfocused window held a permanent stop", err)
	}
	g.applyInput(manual.Frame{Focused: true, Keyboard: manual.Keyboard{Forward: true}})
	if err := g.app.control.Drive(control.Motion{Y: .1}, 300*time.Millisecond, time.Now()); err == nil {
		t.Fatal("held input on refocus did not stop")
	}
}

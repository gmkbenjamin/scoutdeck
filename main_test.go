package main

import (
	"github.com/gmkbenjamin/scoutdeck/internal/control"
	"github.com/gmkbenjamin/scoutdeck/internal/manual"
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

func TestRenderQRImageFillsCanvas(t *testing.T) {
	img, err := renderQRImage("https://scoutdeck-lab.example.ts.net#pair=123456")
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	// Quiet zone corners must be white.
	if c := img.NRGBAAt(0, 0); c.R != 255 || c.G != 255 || c.B != 255 {
		t.Fatalf("expected white quiet zone corner, got %+v", c)
	}
	// Bottom-right quadrant beyond half width/height must contain black QR modules
	// (regression check against rsc.io/qr's unscaled Code.Image bug).
	hasDarkBottomRight := false
	for y := b.Dy() / 2; y < b.Dy(); y++ {
		for x := b.Dx() / 2; x < b.Dx(); x++ {
			if img.NRGBAAt(x, y).R == 0 {
				hasDarkBottomRight = true
				break
			}
		}
	}
	if !hasDarkBottomRight {
		t.Fatalf("QR modules missing from bottom-right quadrant (%dx%d)", b.Dx(), b.Dy())
	}
}

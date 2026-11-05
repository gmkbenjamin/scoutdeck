package control

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"
)

func TestBumpersStopEveryAxis(t *testing.T) {
	for _, speed := range []float64{0, .1, .2, 1, 2} {
		if got := JoystickMotion(.5, .5, .5, speed, true); got != (Motion{}) {
			t.Fatalf("stop at speed %v: %+v", speed, got)
		}
	}
	if got := JoystickMotion(.5, .5, .5, .2, false); got.Y != .1 || got.Z != -1.45 {
		t.Fatal(got)
	}
}

func TestLeaseManualPriorityAndRelease(t *testing.T) {
	c := &Controller{}
	now := time.Now()
	move := Motion{Y: .2}
	if err := c.Drive(move, 300*time.Millisecond, now); err != nil {
		t.Fatal(err)
	}
	if c.command(now).Motion != move || c.command(now.Add(300*time.Millisecond)).Motion != (Motion{}) {
		t.Fatal("lease did not expire")
	}
	c.Manual(move, false, now)
	if err := c.Drive(move, 300*time.Millisecond, now); err == nil {
		t.Fatal("agent overrode human")
	}
	c.Stop()
	c.Manual(move, false, now)
	if c.command(now).Motion != (Motion{}) {
		t.Fatal("held input resumed after stop")
	}
	c.Manual(Motion{}, false, now)
	if err := c.Drive(move, 300*time.Millisecond, now); err != nil {
		t.Fatal(err)
	}
	c.Manual(Motion{Z: 1}, false, now)
	c.Manual(Motion{}, false, now)
	if c.command(now).Motion != (Motion{}) {
		t.Fatal("old agent lease resumed after manual override")
	}
	c.Manual(move, false, now)
	if c.command(now.Add(ManualTimeout)).Motion != (Motion{}) {
		t.Fatal("stale manual input persisted")
	}
}

func TestInvalidAgentCommands(t *testing.T) {
	for _, m := range []Motion{{X: math.NaN()}, {Y: math.Inf(1)}, {X: .21}, {Y: .31}, {Z: 1.81}} {
		if (&Controller{}).Drive(m, 300*time.Millisecond, time.Now()) == nil {
			t.Fatal("accepted", m)
		}
	}
	for _, ttl := range []time.Duration{0, 99 * time.Millisecond, 501 * time.Millisecond, time.Hour} {
		if (&Controller{}).Drive(Motion{}, ttl, time.Now()) == nil {
			t.Fatal("accepted TTL", ttl)
		}
	}
}

func TestRepeatedStopKeepsManualReleaseLatch(t *testing.T) {
	c := &Controller{}
	now := time.Now()
	move := Motion{Y: .2}
	c.Manual(move, false, now)
	c.Stop()
	c.Stop()
	c.Manual(move, false, now)
	if got := c.command(now).Motion; got != (Motion{}) {
		t.Fatalf("repeated stop resumed held movement: %+v", got)
	}
	c.Manual(Motion{}, false, now)
	c.Manual(move, false, now)
	if got := c.command(now).Motion; got != move {
		t.Fatalf("movement did not resume after neutral input: %+v", got)
	}
}

func TestRunStopsOnShutdownAndPublicationFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "publish-error"}[fail], func(t *testing.T) {
			c := &Controller{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_ = c.Drive(Motion{Y: .2}, MaxLease, time.Now())
			ticks := make(chan time.Time)
			writes := make(chan Motion, 4)
			done := make(chan error, 1)
			go func() {
				done <- c.Run(ctx, ticks, func(m Motion) error {
					writes <- m
					if fail && m.Y != 0 {
						return errors.New("disconnected")
					}
					return nil
				})
			}()
			ticks <- time.Now()
			if got := <-writes; got.Y == 0 {
				t.Fatal("expected initial movement")
			}
			cancel()
			select {
			case m := <-writes:
				if m != (Motion{}) {
					t.Fatal("final command was not zero", m)
				}
			case <-time.After(time.Second):
				t.Fatal("no final stop")
			}
			<-done
			if c.Drive(Motion{}, MaxLease, time.Now()) == nil {
				t.Fatal("accepted command after shutdown")
			}
		})
	}
}

func TestSlowActionsDoNotBlockDriveOrStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := NewActions()
	started := make(chan struct{})
	finished := make(chan struct{})
	go a.Run(ctx, func(error) { close(finished) })
	deadline := time.Now().Add(time.Second)
	for a.Submit(func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }) != nil {
		if time.Now().After(deadline) {
			t.Fatal("worker not ready")
		}
		time.Sleep(time.Millisecond)
	}
	<-started
	if a.Submit(func(context.Context) error { return nil }) == nil {
		t.Fatal("queued a duplicate while busy")
	}
	c := &Controller{}
	_ = c.Drive(Motion{Y: .2}, MaxLease, time.Now())
	writes := make(chan Motion, 4)
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, ticks, func(m Motion) error { writes <- m; return nil }) }()
	ticks <- time.Now()
	if (<-writes).Y == 0 {
		t.Fatal("movement blocked")
	}
	c.Stop()
	ticks <- time.Now()
	if (<-writes) != (Motion{}) {
		t.Fatal("stop blocked")
	}
	cancel()
	<-done
	<-finished
}

func TestScreenshotNoFrameAndCachedFrame(t *testing.T) {
	m := &Media{}
	if _, err := m.Save(t.TempDir()); err == nil {
		t.Fatal("missing frame should return an error")
	}
	data := []byte{1, 2, 3}
	m.PutFrame(data)
	data[0] = 9
	dir := t.TempDir()
	a, err := m.Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("screenshots overwrite each other")
	}
	got, err := os.ReadFile(a)
	if err != nil || got[0] != 1 {
		t.Fatal("frame not copied", got, err)
	}
	f := m.Frame()
	f.Data[0] = 8
	if m.Frame().Data[0] != 1 {
		t.Fatal("frame exposed mutable data")
	}
}

func TestConcurrentStateAndMedia(t *testing.T) {
	c := &Controller{}
	m := &Media{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Manual(Motion{}, false, time.Now())
				_ = c.Drive(Motion{}, MaxLease, time.Now())
				c.Stop()
				_ = c.Snapshot()
				m.PutFrame([]byte{1, 2, 3})
				_ = m.Frame()
				ch, unsub := m.SubscribeAudio()
				m.Audio([]byte{4})
				select {
				case <-ch:
				default:
				}
				unsub()
			}
		}()
	}
	wg.Wait()
}

func TestDockingPreservesHeldManualStop(t *testing.T) {
	var c Controller
	now := time.Now()
	c.Manual(Motion{}, true, now)
	if err := c.CheckAutonomy(now); err == nil {
		t.Fatal("docking bypassed held stop")
	}
	c.Manual(Motion{Y: .1}, false, now)
	if err := c.CheckAutonomy(now); err == nil {
		t.Fatal("docking bypassed manual input")
	}
	c.Manual(Motion{}, false, now)
	if err := c.CheckAutonomy(now); err != nil {
		t.Fatal(err)
	}
}

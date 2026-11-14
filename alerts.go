package main

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"log"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/aler9/goroslib"
	"github.com/shell-company/scout/internal/events"
)

// Guard mode watches the camera while the robot sits still: the Mac compares
// frames for motion, and the robot's own model reports people, cats and dogs.
// Each alert is logged with a snapshot, shown in the HUD and web app, and
// optionally raised as a macOS notification.

type guard struct {
	mu       sync.Mutex
	detector events.Detector
	last     map[string]time.Time // last alert per kind
	box      *events.Box          // latest motion box, for the HUD
	boxAt    time.Time
	frac     float64
	objSub   *goroslib.Subscriber
}

func (a *application) guardSettings() GuardSettings {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	return a.settings.Guard.normalized()
}

func (a *application) setGuard(g GuardSettings) error {
	g = g.normalized()
	a.settingsMu.Lock()
	was := a.settings.Guard.Armed
	a.settings.Guard = g
	a.settingsMu.Unlock()
	if err := a.saveSettings(); err != nil {
		return err
	}
	a.guard.mu.Lock()
	a.guard.detector.Sensitivity = g.Sensitivity
	a.guard.detector.Reset()
	a.guard.mu.Unlock()
	if g.Armed != was {
		if g.Armed {
			a.logEvent("info", "Guard mode armed", false)
		} else {
			a.logEvent("info", "Guard mode disarmed", false)
		}
	}
	a.syncObjectDetector()
	return nil
}

// syncObjectDetector subscribes to the robot's people/pet detector only
// while guard mode wants it: the robot runs the model only with a subscriber.
func (a *application) syncObjectDetector() {
	a.guard.mu.Lock()
	defer a.guard.mu.Unlock()
	g := a.guardSettings()
	want := g.Armed && g.Objects && a.ready.Load()
	switch {
	case want && a.guard.objSub == nil:
		node := rosNode()
		if node == nil {
			return
		}
		s, err := goroslib.NewSubscriber(goroslib.SubscriberConf{Node: node, Topic: "/CoreNode/obj", QueueSize: 4, Callback: a.onObject})
		if err != nil {
			log.Printf("people/pet detection unavailable: %v", err)
			return
		}
		a.guard.objSub = s
	case !want && a.guard.objSub != nil:
		_ = a.guard.objSub.Close()
		a.guard.objSub = nil
	}
}

func (a *application) closeObjectDetector() {
	a.guard.mu.Lock()
	defer a.guard.mu.Unlock()
	if a.guard.objSub != nil {
		_ = a.guard.objSub.Close()
		a.guard.objSub = nil
	}
}

func (a *application) onObject(m *detectMsg) {
	kind := m.Name
	if kind != "person" && kind != "cat" && kind != "dog" || m.Score < 0.55 {
		return
	}
	box := [4]float64{float64(m.Left), float64(m.Top), float64(m.Right), float64(m.Bottom)}
	// Coordinates are pixels on some firmware builds; normalise them.
	if box[2] > 1.5 || box[3] > 1.5 {
		w, h := float64(max(m.Width, 1)), float64(max(m.Height, 1))
		box = [4]float64{box[0] / w, box[1] / h, box[2] / w, box[3] / h}
	}
	now := time.Now()
	a.sensors.addDetection(detection{Name: kind, Score: float64(m.Score), Box: box, At: now})
	a.alert(kind, fmt.Sprintf("%s detected (%.0f%%)", events.Label(kind), m.Score*100), float64(m.Score),
		&events.Box{Left: box[0], Top: box[1], Right: box[2], Bottom: box[3]})
}

// alert logs and announces one detection, respecting the per-kind cooldown.
func (a *application) alert(kind, text string, score float64, box *events.Box) {
	g := a.guardSettings()
	if !g.Armed {
		return
	}
	now := time.Now()
	a.guard.mu.Lock()
	if a.guard.last == nil {
		a.guard.last = map[string]time.Time{}
	}
	if now.Sub(a.guard.last[kind]) < time.Duration(g.Cooldown)*time.Second {
		a.guard.mu.Unlock()
		return
	}
	a.guard.last[kind] = now
	a.guard.mu.Unlock()
	f := media.Frame()
	e, err := a.events.Add(kind, text, f.Data, score, box)
	if err != nil {
		log.Printf("event log: %v", err)
	}
	log.Printf("alert: %s", text)
	a.notices.add(text, noticeBad)
	if g.Notify {
		notifyMac("Scout: "+events.Label(kind), text+" at "+e.At.Format("15:04:05"))
	}
}

// logEvent records an informational event (patrols, docking, guard changes).
func (a *application) logEvent(kind, text string, snapshot bool) {
	var jpg []byte
	if snapshot {
		if f := media.Frame(); time.Since(f.At) < 3*time.Second {
			jpg = f.Data
		}
	}
	if _, err := a.events.Add(kind, text, jpg, 0, nil); err != nil {
		log.Printf("event log: %v", err)
	}
}

// watchGuard compares camera frames for motion twice a second while armed.
func (a *application) watchGuard(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var lastFrame time.Time
	for {
		select {
		case <-ctx.Done():
			a.closeObjectDetector()
			return
		case <-t.C:
		}
		g := a.guardSettings()
		if !g.Armed {
			continue
		}
		f := media.Frame()
		if f.At.Equal(lastFrame) || time.Since(f.At) > 2*time.Second {
			continue
		}
		lastFrame = f.At
		img, err := jpeg.Decode(bytes.NewReader(f.Data))
		if err != nil {
			continue
		}
		owner := a.control.Snapshot().Owner
		moving := owner != "stopped" && owner != "" || a.dockingActive() || a.act.snapshot(a.sensors.tracker.Pose()).Kind != ""
		a.guard.mu.Lock()
		a.guard.detector.Sensitivity = g.Sensitivity
		frac, box := a.guard.detector.Feed(img, moving, time.Now())
		a.guard.frac = frac
		if box != nil {
			a.guard.box, a.guard.boxAt = box, time.Now()
		}
		a.guard.mu.Unlock()
		if box != nil {
			a.alert("motion", fmt.Sprintf("Motion detected (%.1f%% of the view)", frac*100), frac, box)
		}
	}
}

// motionBox returns the latest motion box if it is recent.
func (a *application) motionBox(now time.Time) *events.Box {
	a.guard.mu.Lock()
	defer a.guard.mu.Unlock()
	if a.guard.box == nil || now.Sub(a.guard.boxAt) > 1500*time.Millisecond {
		return nil
	}
	b := *a.guard.box
	return &b
}

// notifyMac shows a macOS notification. Text goes as osascript arguments,
// never through a shell or into the script source.
var notifySlots = make(chan struct{}, 3)

func notifyMac(title, text string) {
	if runtime.GOOS != "darwin" {
		return
	}
	select {
	case notifySlots <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-notifySlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "osascript",
			"-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv) sound name \"Glass\"", "-e", "end run",
			title, text)
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			log.Printf("notification: %v", err)
		}
	}()
}

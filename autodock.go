package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Auto-dock sends the robot to its charger when the battery runs low, while
// the Scout app is connected. It warns first and backs off when someone is
// driving, so it never takes the controls away mid-use.

const (
	autoDockDefault   = 20
	autoDockMin       = 10
	autoDockMax       = 50
	autoDockWarning   = 30 * time.Second
	autoDockBackoff   = 5 * time.Minute
	autoDockAttempts  = 3
	batteryFreshFor   = 90 * time.Second
	autoDockRearmGain = 5 // percent above the threshold that re-arms a cycle
)

// autoDockInput is everything the decision needs, sampled once per tick.
type autoDockInput struct {
	Enabled     bool
	Threshold   int
	Percent     int
	ChargeState int // 0 charging, 1 on battery, 2 full, 3 unknown
	BatteryAt   time.Time
	CanDock     bool // connected, with SSH charging checks
	Docking     bool // a docking operation is running
	Driving     bool // someone is driving or holding stop
	Now         time.Time
}

type autoDockState struct {
	DueAt      time.Time // countdown end; zero when not counting down
	NotBefore  time.Time // backoff after driving or a failed attempt
	Attempts   int       // attempts in this discharge cycle
	LastResult string
}

type autoDockAction int

const (
	adNothing autoDockAction = iota
	adWarn                   // start the countdown
	adCancel                 // countdown cancelled (someone drove)
	adDock                   // start docking now
)

// decideAutoDock is pure so it can be tested without a robot.
func decideAutoDock(in autoDockInput, st *autoDockState) autoDockAction {
	onBattery := in.ChargeState == 1
	fresh := !in.BatteryAt.IsZero() && in.Now.Sub(in.BatteryAt) < batteryFreshFor && in.Percent >= 0
	// Re-arm once the robot is charging again or the battery is comfortably above the threshold.
	if fresh && (!onBattery || in.Percent > in.Threshold+autoDockRearmGain) {
		*st = autoDockState{}
		return adNothing
	}
	if !in.Enabled || !in.CanDock || !fresh || !onBattery || in.Percent > in.Threshold || in.Docking {
		if !st.DueAt.IsZero() {
			st.DueAt = time.Time{}
			return adCancel
		}
		return adNothing
	}
	if st.Attempts >= autoDockAttempts || in.Now.Before(st.NotBefore) {
		return adNothing
	}
	if in.Driving {
		if !st.DueAt.IsZero() {
			st.DueAt, st.NotBefore = time.Time{}, in.Now.Add(autoDockBackoff)
			return adCancel
		}
		return adNothing
	}
	if st.DueAt.IsZero() {
		st.DueAt = in.Now.Add(autoDockWarning)
		return adWarn
	}
	if in.Now.Before(st.DueAt) {
		return adNothing
	}
	st.DueAt, st.NotBefore = time.Time{}, in.Now.Add(autoDockBackoff)
	st.Attempts++
	return adDock
}

type autoDocker struct {
	mu    sync.Mutex
	state autoDockState
}

// AutoDockSettings is the user-facing configuration.
type AutoDockSettings struct {
	Enabled   bool `json:"enabled"`
	Threshold int  `json:"threshold"`
}

func (a *application) autoDockSettings() AutoDockSettings {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	t := a.settings.AutoDockAt
	if t < autoDockMin || t > autoDockMax {
		t = autoDockDefault
	}
	return AutoDockSettings{Enabled: a.settings.AutoDock, Threshold: t}
}

func (a *application) setAutoDock(s AutoDockSettings) error {
	if s.Threshold < autoDockMin || s.Threshold > autoDockMax {
		return fmt.Errorf("threshold must be %d..%d%%", autoDockMin, autoDockMax)
	}
	a.settingsMu.Lock()
	a.settings.AutoDock, a.settings.AutoDockAt = s.Enabled, s.Threshold
	saved := a.settings
	a.settingsMu.Unlock()
	if err := saveSettings(saved); err != nil {
		return err
	}
	if !s.Enabled {
		a.autoDock.mu.Lock()
		a.autoDock.state = autoDockState{}
		a.autoDock.mu.Unlock()
	}
	log.Printf("auto-dock: enabled=%v threshold=%d%%", s.Enabled, s.Threshold)
	return nil
}

// autoDockCountdown returns the seconds left before auto-docking, or 0.
func (a *application) autoDockCountdown() int {
	a.autoDock.mu.Lock()
	defer a.autoDock.mu.Unlock()
	if a.autoDock.state.DueAt.IsZero() {
		return 0
	}
	return max(1, int(time.Until(a.autoDock.state.DueAt).Seconds()+0.99))
}

func (a *application) watchAutoDock(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg := a.autoDockSettings()
		batteryMu.RLock()
		pct, charge, at := batteryPercent, batteryChargeState, batteryAt
		batteryMu.RUnlock()
		ctrl := a.control.Snapshot()
		in := autoDockInput{
			Enabled: cfg.Enabled, Threshold: cfg.Threshold, Percent: pct, ChargeState: charge, BatteryAt: at,
			CanDock: a.ready.Load() && a.audio != nil, Docking: a.dock != nil && a.dock.Active(),
			Driving: ctrl.Owner == "manual" || ctrl.Owner == "manual-stop" || ctrl.Owner == "agent",
			Now:     time.Now(),
		}
		a.autoDock.mu.Lock()
		action := decideAutoDock(in, &a.autoDock.state)
		attempt := a.autoDock.state.Attempts
		a.autoDock.mu.Unlock()
		switch action {
		case adWarn:
			msg := fmt.Sprintf("Battery %d%% - auto-docking in %ds. Drive or press Stop to postpone.", pct, int(autoDockWarning/time.Second))
			log.Print("auto-dock: " + msg)
			a.notices.add(msg, noticeBad)
		case adCancel:
			a.notices.add("Auto-dock postponed for 5 minutes", noticeInfo)
		case adDock:
			if _, err := a.startDock(ctx, 300*time.Second); err != nil {
				log.Printf("auto-dock attempt %d: %v", attempt, err)
				a.notices.add("Auto-dock could not start: "+err.Error(), noticeBad)
				continue
			}
			log.Printf("auto-dock: battery %d%%, docking (attempt %d of %d)", pct, attempt, autoDockAttempts)
			a.notices.add(fmt.Sprintf("Low battery (%d%%): returning to the dock", pct), noticeInfo)
		}
	}
}

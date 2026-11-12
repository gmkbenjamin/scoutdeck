package main

import (
	"testing"
	"time"
)

func lowBattery(now time.Time) autoDockInput {
	return autoDockInput{Enabled: true, Threshold: 20, Percent: 18, ChargeState: 1, BatteryAt: now, CanDock: true, Now: now}
}

func TestAutoDockWarnsThenDocks(t *testing.T) {
	now := time.Now()
	var st autoDockState
	if a := decideAutoDock(lowBattery(now), &st); a != adWarn {
		t.Fatal("expected warning", a)
	}
	if a := decideAutoDock(lowBattery(now.Add(10*time.Second)), &st); a != adNothing {
		t.Fatal("docked before the countdown ended", a)
	}
	if a := decideAutoDock(lowBattery(now.Add(autoDockWarning)), &st); a != adDock || st.Attempts != 1 {
		t.Fatal("expected dock", a, st)
	}
	// Still low after a failed attempt: wait for the backoff, then warn again.
	if a := decideAutoDock(lowBattery(now.Add(autoDockWarning+time.Minute)), &st); a != adNothing {
		t.Fatal("retried during backoff", a)
	}
	if a := decideAutoDock(lowBattery(now.Add(autoDockWarning+autoDockBackoff+time.Second)), &st); a != adWarn {
		t.Fatal("expected retry warning", a)
	}
}

func TestAutoDockGivesUpAfterAttemptsAndRearmsWhenCharging(t *testing.T) {
	now := time.Now()
	st := autoDockState{Attempts: autoDockAttempts}
	if a := decideAutoDock(lowBattery(now), &st); a != adNothing {
		t.Fatal("exceeded attempts", a)
	}
	charging := lowBattery(now)
	charging.ChargeState = 0
	decideAutoDock(charging, &st)
	if st.Attempts != 0 {
		t.Fatal("charging should re-arm", st)
	}
}

func TestAutoDockYieldsToDriversAndConditions(t *testing.T) {
	now := time.Now()
	var st autoDockState
	decideAutoDock(lowBattery(now), &st)
	driving := lowBattery(now.Add(5 * time.Second))
	driving.Driving = true
	if a := decideAutoDock(driving, &st); a != adCancel || !st.NotBefore.After(now.Add(4*time.Minute)) {
		t.Fatal("driving should postpone", a, st)
	}
	for name, mutate := range map[string]func(*autoDockInput){
		"disabled":     func(in *autoDockInput) { in.Enabled = false },
		"no ssh":       func(in *autoDockInput) { in.CanDock = false },
		"stale":        func(in *autoDockInput) { in.BatteryAt = in.Now.Add(-5 * time.Minute) },
		"above":        func(in *autoDockInput) { in.Percent = 21 },
		"docking":      func(in *autoDockInput) { in.Docking = true },
		"unknown":      func(in *autoDockInput) { in.Percent = -1 },
		"full/charged": func(in *autoDockInput) { in.ChargeState = 2 },
	} {
		var st autoDockState
		in := lowBattery(now)
		mutate(&in)
		if a := decideAutoDock(in, &st); a != adNothing {
			t.Errorf("%s: %v", name, a)
		}
	}
}

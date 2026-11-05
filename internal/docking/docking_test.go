package docking

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(ctx context.Context) (*Manager, *atomic.Int32, *atomic.Int32, *atomic.Bool) {
	starts, cancels, connected := &atomic.Int32{}, &atomic.Int32{}, &atomic.Bool{}
	m := New(ctx, Hooks{
		Prepare: func(context.Context) error { return nil },
		Start:   func(context.Context) error { starts.Add(1); return nil },
		Cancel:  func(context.Context) error { cancels.Add(1); return nil },
		Probe: func(context.Context) (Power, error) {
			return Power{Connected: connected.Load(), ObservedAt: time.Now()}, nil
		},
	})
	m.interval = time.Millisecond
	return m, starts, cancels, connected
}
func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestSuccessRequiresPowerAndReleasesControl(t *testing.T) {
	m, starts, cancels, power := fixture(context.Background())
	if _, err := m.Start(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return starts.Load() == 1 })
	m.Phase(4)
	time.Sleep(10 * time.Millisecond)
	if !m.Active() || m.Snapshot().ChargingVerified {
		t.Fatal("firmware success alone was accepted")
	}
	if err := m.WhileIdle(func() error { t.Error("drive reached controller during docking"); return nil }); err == nil {
		t.Fatal("drive accepted")
	}
	if _, err := m.Start(10 * time.Second); err == nil {
		t.Fatal("duplicate start accepted")
	}
	power.Store(true)
	await(t, func() bool { return !m.Active() })
	op := m.Snapshot()
	if op.State != "charging" || !op.ChargingVerified || cancels.Load() != 1 {
		t.Fatal(op, cancels.Load())
	}
	if err := m.WhileIdle(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestAlreadyPoweredDoesNotMove(t *testing.T) {
	m, starts, _, power := fixture(context.Background())
	power.Store(true)
	if _, err := m.Start(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return !m.Active() })
	if starts.Load() != 0 || !m.Snapshot().ChargingVerified {
		t.Fatal(m.Snapshot(), starts.Load())
	}
}
func TestCancelAndDeadlineStopNativeDocking(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "timeout"}[deadline], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			m, starts, cancels, _ := fixture(ctx)
			if _, err := m.Start(10 * time.Second); err != nil {
				t.Fatal(err)
			}
			await(t, func() bool { return starts.Load() == 1 })
			want := "timed_out"
			if !deadline {
				want = "cancelled"
				if err := m.Cancel(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			await(t, func() bool { return !m.Active() })
			if m.Snapshot().State != want || cancels.Load() != 1 {
				t.Fatal(m.Snapshot(), cancels.Load())
			}
		})
	}
}
func TestCancellationFailureBlocksDriveUntilRetry(t *testing.T) {
	m, starts, _, _ := fixture(context.Background())
	var fail atomic.Bool
	fail.Store(true)
	m.hooks.Cancel = func(context.Context) error {
		if fail.Load() {
			return errors.New("offline")
		}
		return nil
	}
	if _, err := m.Start(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return starts.Load() == 1 })
	if err := m.Cancel(context.Background()); err == nil {
		t.Fatal("hidden cancel failure")
	}
	m.Phase(6)
	if m.Snapshot().State != "cancel_failed" || !m.Active() {
		t.Fatal(m.Snapshot())
	}
	fail.Store(false)
	if err := m.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Active() {
		t.Fatal(m.Snapshot())
	}
}
func TestUnknownPowerFailsBeforeMovement(t *testing.T) {
	m, starts, _, _ := fixture(context.Background())
	m.hooks.Probe = func(context.Context) (Power, error) { return Power{}, errors.New("unknown battery status") }
	if _, err := m.Start(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return !m.Active() })
	if starts.Load() != 0 || m.Snapshot().State != "failed" || m.Snapshot().ChargingVerified {
		t.Fatal(m.Snapshot())
	}
}
func TestFirmwareFailureStopsWithoutSuccess(t *testing.T) {
	m, starts, cancels, _ := fixture(context.Background())
	if _, err := m.Start(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return starts.Load() == 1 })
	m.Phase(5)
	await(t, func() bool { return !m.Active() })
	if m.Snapshot().State != "failed" || cancels.Load() != 1 || m.Snapshot().ChargingVerified {
		t.Fatal(m.Snapshot())
	}
}

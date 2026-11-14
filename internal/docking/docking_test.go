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

func TestFirmwareStopEndsOperationOnlyAfterItStarted(t *testing.T) {
	for _, phase := range []int32{6, 7} {
		m, starts, cancels, _ := fixture(context.Background())
		if _, err := m.Start(10 * time.Second); err != nil {
			t.Fatal(err)
		}
		await(t, func() bool { return starts.Load() == 1 })
		// An idle report before the firmware starts working must be ignored.
		m.Phase(phase)
		time.Sleep(20 * time.Millisecond)
		if !m.Active() {
			t.Fatalf("phase %d before start ended docking: %+v", phase, m.Snapshot())
		}
		m.Phase(1)
		m.Phase(phase)
		await(t, func() bool { return !m.Active() })
		if op := m.Snapshot(); op.State != "failed" || op.ChargingVerified || cancels.Load() != 1 {
			t.Fatal(phase, op)
		}
	}
}
func TestFirmwareStopWithPowerStillSucceeds(t *testing.T) {
	m, starts, _, power := fixture(context.Background())
	m.interval = 5 * time.Millisecond
	if _, err := m.Start(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return starts.Load() == 1 })
	m.Phase(3)
	power.Store(true)
	m.Phase(6)
	await(t, func() bool { return !m.Active() })
	if op := m.Snapshot(); op.State != "charging" || !op.ChargingVerified {
		t.Fatal(op)
	}
}
func TestTimeoutAllowsSlowFirmware(t *testing.T) {
	m, _, _, _ := fixture(context.Background())
	if _, err := m.Start(MaxTimeout + time.Second); err == nil {
		t.Fatal("timeout above maximum accepted")
	}
	if _, err := m.Start(300 * time.Second); err != nil {
		t.Fatal(err)
	}
	_ = m.Cancel(context.Background())
}

func TestApproachPhaseTransitionsAndReleases(t *testing.T) {
	m, starts, cancels, power := fixture(context.Background())
	inApproach := make(chan struct{})
	releaseApproach := make(chan struct{})
	m.hooks.ShouldApproach = func() bool { return true }
	m.hooks.Approach = func(ctx context.Context) error {
		if m.NativeActive() {
			t.Error("NativeActive during approach")
		}
		if err := m.WhileIdle(func() error { return nil }); err == nil {
			t.Error("WhileIdle succeeded during approach")
		}
		if err := m.WhileApproaching(func() error { return nil }); err != nil {
			t.Errorf("WhileApproaching failed: %v", err)
		}
		close(inApproach)
		select {
		case <-releaseApproach:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	op, err := m.Start(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "approaching" || !op.Active {
		t.Fatalf("Start blocked or wrong initial state: %+v", op)
	}
	<-inApproach
	close(releaseApproach)
	await(t, func() bool { return starts.Load() == 1 && m.NativeActive() })
	power.Store(true)
	await(t, func() bool { return !m.Active() })
	if op := m.Snapshot(); op.State != "charging" || !op.ChargingVerified || cancels.Load() != 1 {
		t.Fatalf("final op %+v cancels=%d", op, cancels.Load())
	}
}

func TestCancelDuringApproachDoesNotCallNativeCancel(t *testing.T) {
	m, starts, cancels, _ := fixture(context.Background())
	inApproach := make(chan struct{})
	m.hooks.ShouldApproach = func() bool { return true }
	m.hooks.Cancel = func(context.Context) error {
		cancels.Add(1)
		return errors.New("should not call native cancel before native start")
	}
	m.hooks.Approach = func(ctx context.Context) error {
		close(inApproach)
		<-ctx.Done()
		return ctx.Err()
	}
	if _, err := m.Start(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	<-inApproach
	if err := m.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if op := m.Snapshot(); op.State != "cancelled" || op.Active || starts.Load() != 0 || cancels.Load() != 0 {
		t.Fatalf("unexpected state: %+v starts=%d cancels=%d", op, starts.Load(), cancels.Load())
	}
}

func TestContextCancelAndTimeoutDuringPrepareAndProbe(t *testing.T) {
	t.Run("cancel_in_prepare", func(t *testing.T) {
		m, starts, cancels, _ := fixture(context.Background())
		inPrepare := make(chan struct{})
		m.hooks.Prepare = func(ctx context.Context) error {
			close(inPrepare)
			<-ctx.Done()
			return ctx.Err()
		}
		if _, err := m.Start(10 * time.Second); err != nil {
			t.Fatal(err)
		}
		<-inPrepare
		if err := m.Cancel(context.Background()); err != nil {
			t.Fatal(err)
		}
		if op := m.Snapshot(); op.State != "cancelled" || op.Active || starts.Load() != 0 || cancels.Load() != 0 {
			t.Fatalf("unexpected state: %+v starts=%d cancels=%d", op, starts.Load(), cancels.Load())
		}
	})

	t.Run("cancel_in_initial_probe", func(t *testing.T) {
		m, starts, cancels, _ := fixture(context.Background())
		inProbe := make(chan struct{})
		m.hooks.Probe = func(ctx context.Context) (Power, error) {
			close(inProbe)
			<-ctx.Done()
			return Power{}, ctx.Err()
		}
		if _, err := m.Start(10 * time.Second); err != nil {
			t.Fatal(err)
		}
		<-inProbe
		if err := m.Cancel(context.Background()); err != nil {
			t.Fatal(err)
		}
		if op := m.Snapshot(); op.State != "cancelled" || op.Active || starts.Load() != 0 || cancels.Load() != 0 {
			t.Fatalf("unexpected state: %+v starts=%d cancels=%d", op, starts.Load(), cancels.Load())
		}
	})

	t.Run("timeout_in_initial_probe", func(t *testing.T) {
		pctx, pcancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer pcancel()
		pm, pstarts, pcancels, _ := fixture(pctx)
		pm.hooks.Probe = func(ctx context.Context) (Power, error) {
			<-ctx.Done()
			return Power{}, ctx.Err()
		}
		if _, err := pm.Start(10 * time.Second); err != nil {
			t.Fatal(err)
		}
		await(t, func() bool { return !pm.Active() })
		if op := pm.Snapshot(); op.State != "timed_out" || op.Active || pstarts.Load() != 0 || pcancels.Load() != 0 {
			t.Fatalf("unexpected timeout state: %+v starts=%d cancels=%d", op, pstarts.Load(), pcancels.Load())
		}
	})

	t.Run("cancel_in_deferred_startNative", func(t *testing.T) {
		m, starts, cancels, power := fixture(context.Background())
		power.Store(true)
		inStart := make(chan struct{})
		probes := 0
		m.hooks.Probe = func(context.Context) (Power, error) {
			probes++
			// Initial probe looks connected so native start is deferred to the polling loop;
			// the second probe drops power so startNative runs in the polling branch.
			return Power{Connected: probes == 1}, nil
		}
		m.hooks.Start = func(ctx context.Context) error {
			starts.Add(1)
			close(inStart)
			<-ctx.Done()
			return ctx.Err()
		}
		if _, err := m.Start(10 * time.Second); err != nil {
			t.Fatal(err)
		}
		<-inStart
		if err := m.Cancel(context.Background()); err != nil {
			t.Fatal(err)
		}
		if op := m.Snapshot(); op.State != "cancelled" || op.Active || starts.Load() != 1 || cancels.Load() != 1 {
			t.Fatalf("unexpected deferred cancel state: %+v starts=%d cancels=%d", op, starts.Load(), cancels.Load())
		}
	})
}

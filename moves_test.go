package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmkbenjamin/scoutdeck/internal/control"
	"github.com/gmkbenjamin/scoutdeck/internal/docking"
	"github.com/gmkbenjamin/scoutdeck/internal/events"
	"github.com/gmkbenjamin/scoutdeck/internal/motion"
)

func TestActivityIntentAxes(t *testing.T) {
	now := time.Now()
	var ac activity
	if m, ok := ac.intent(); ok || m != (control.Motion{}) {
		t.Fatalf("empty activity intent = (%+v, %v), want zero/false", m, ok)
	}

	cases := []struct {
		name string
		goal motion.Goal
		want control.Motion
	}{
		{name: "drive_forward", goal: motion.Goal{Kind: motion.Drive, Target: 0.5, Speed: 0.2}, want: control.Motion{Y: 0.2}},
		{name: "drive_back", goal: motion.Goal{Kind: motion.Drive, Target: -0.5, Speed: 0.2}, want: control.Motion{Y: -0.2}},
		{name: "strafe_left", goal: motion.Goal{Kind: motion.Strafe, Target: 0.5, Speed: 0.15}, want: control.Motion{X: -0.15}},
		{name: "strafe_right", goal: motion.Goal{Kind: motion.Strafe, Target: -0.5, Speed: 0.15}, want: control.Motion{X: 0.15}},
		{name: "turn_left", goal: motion.Goal{Kind: motion.Turn, Target: 1.0, Speed: 1.0}, want: control.Motion{Z: 1.0}},
		{name: "turn_right", goal: motion.Goal{Kind: motion.Turn, Target: -1.0, Speed: 1.0}, want: control.Motion{Z: -1.0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ac.begin("move", tc.name, nil); err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer ac.end(nil)
			ac.setMover(motion.NewMover(tc.goal, motion.Pose{}, now))
			got, ok := ac.intent()
			if !ok || got != tc.want {
				t.Fatalf("intent() = (%+v, %v), want (%+v, true)", got, ok, tc.want)
			}
		})
	}

	if err := ac.begin("patrol", "p", nil); err != nil {
		t.Fatalf("begin patrol: %v", err)
	}
	defer ac.end(nil)
	ac.setMover(motion.NewMover(motion.Goal{Kind: motion.Drive, Target: 1, Speed: 0.2}, motion.Pose{}, now))
	if m, ok := ac.intent(); ok || m != (control.Motion{}) {
		t.Fatalf("patrol intent = (%+v, %v), want zero/false", m, ok)
	}
}

func newTestApp(t *testing.T) *application {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := &application{
		ctx:        ctx,
		cancel:     cancel,
		connCtx:    ctx,
		connCancel: cancel,
		events:     &events.Log{Dir: t.TempDir()},
	}
	a.ready.Store(true)
	a.setDockManager(docking.New(ctx, docking.Hooks{
		Prepare: func(context.Context) error { return nil },
		Start:   func(context.Context) error { return nil },
		Cancel:  func(context.Context) error { return nil },
		Probe:   func(context.Context) (docking.Power, error) { return docking.Power{}, nil },
	}))
	return a
}

func TestRunRouteStopsOnObstacleCancelRetryStallAndPatrolDockAfter(t *testing.T) {
	t.Run("obstacle_wait_cancel", func(t *testing.T) {
		a := newTestApp(t)
		now := time.Now()
		a.sensors.tracker.Command(control.Motion{Y: 0.2}, now.Add(-500*time.Millisecond))
		a.sensors.tracker.Odometry(0.2, 0, now.Add(-480*time.Millisecond))
		a.sensors.tracker.Command(control.Motion{}, now.Add(-460*time.Millisecond))
		a.sensors.tracker.Reset(now.Add(-450 * time.Millisecond))
		a.sensors.setRange(0.10, now.Add(-40*time.Millisecond))
		a.sensors.setRange(0.10, now)

		route := motion.Route{
			Name: "blocked",
			Steps: []motion.Step{
				{Motion: control.Motion{Y: 0.2}, Distance: 0.5, Seconds: 3},
				{Motion: control.Motion{}, Seconds: 2},
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		resCh := make(chan motion.Result, 1)
		started := time.Now()
		go func() { resCh <- a.runRoute(ctx, route) }()

		deadline := time.Now().Add(time.Second)
		for {
			a.act.mu.Lock()
			inWait := a.act.step == 1 && a.act.mover == nil
			a.act.mu.Unlock()
			if inWait {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("route did not reach obstacle wait")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()

		select {
		case res := <-resCh:
			if res.Completed || res.StoppedBy != "cancelled" || res.Achieved != 0 {
				t.Fatalf("runRoute = %+v, want StoppedBy=cancelled Achieved=0 Completed=false", res)
			}
			if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
				t.Fatalf("cancelled route took %v, expected prompt abort", elapsed)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("runRoute did not return after cancel during obstacle wait")
		}
	})

	t.Run("obstacle_retry_completes", func(t *testing.T) {
		a := newTestApp(t)
		now := time.Now()
		a.sensors.tracker.Command(control.Motion{Y: 0.2}, now.Add(-500*time.Millisecond))
		a.sensors.tracker.Odometry(0.2, 0, now.Add(-480*time.Millisecond))
		a.sensors.tracker.Command(control.Motion{}, now.Add(-460*time.Millisecond))
		a.sensors.tracker.Reset(now.Add(-450 * time.Millisecond))
		a.sensors.setRange(0.10, now.Add(-40*time.Millisecond))
		a.sensors.setRange(0.10, now)

		route := motion.Route{
			Name: "retry",
			Steps: []motion.Step{
				{Motion: control.Motion{Y: 0.2}, Distance: 0.15, Seconds: 1},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()

		resCh := make(chan motion.Result, 1)
		go func() { resCh <- a.runRoute(ctx, route) }()

		deadline := time.Now().Add(time.Second)
		for {
			a.act.mu.Lock()
			inWait := a.act.step == 1 && a.act.mover == nil
			a.act.mu.Unlock()
			if inWait {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("route did not enter obstacle wait")
			}
			time.Sleep(5 * time.Millisecond)
		}

		feedStop := make(chan struct{})
		go func() {
			tk := time.NewTicker(20 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-feedStop:
					return
				case now := <-tk.C:
					a.sensors.setRange(math.Inf(1), now)
					a.sensors.tracker.Command(control.Motion{Y: 0.2}, now)
					a.sensors.tracker.Odometry(0.2, 0, now)
				}
			}
		}()
		defer close(feedStop)

		select {
		case res := <-resCh:
			if !res.Completed || res.StoppedBy != "" || res.Achieved != 1 {
				t.Fatalf("retry runRoute = %+v, want Completed=true Achieved=1", res)
			}
		case <-ctx.Done():
			t.Fatal("obstacle retry did not complete in time")
		}
	})

	t.Run("stalled_aborts_route", func(t *testing.T) {
		a := newTestApp(t)
		route := motion.Route{
			Name: "stalled",
			Steps: []motion.Step{
				{Motion: control.Motion{Z: 1.0}, Turn: 1.0, Seconds: 1},
				{Motion: control.Motion{}, Seconds: 0.2},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()

		res := a.runRoute(ctx, route)
		if res.Completed || res.StoppedBy != "stalled" || res.Achieved != 0 {
			t.Fatalf("runRoute = %+v, want StoppedBy=stalled Achieved=0 Completed=false", res)
		}
	})

	t.Run("patrol_dock_after_starts_dock", func(t *testing.T) {
		a := newTestApp(t)
		r := motion.Route{
			Name:      "short",
			DockAfter: true,
			Steps:     []motion.Step{{Motion: control.Motion{}, Seconds: 0.05}},
		}
		if err := a.routes.put(r); err != nil {
			t.Fatalf("put route: %v", err)
		}
		if err := a.Patrol(context.Background(), "short"); err != nil {
			t.Fatalf("Patrol: %v", err)
		}
		d := a.dockManager()
		deadline := time.Now().Add(2 * time.Second)
		for !d.Active() {
			if time.Now().After(deadline) {
				t.Fatal("Patrol with DockAfter did not start docking")
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = d.Cancel(context.Background())
		a.opWG.Wait()
	})
}

func TestApproachDockResultNormalization(t *testing.T) {
	t.Run("already_in_position", func(t *testing.T) {
		a := newTestApp(t)
		t0 := time.Now().Add(-2 * time.Second)
		a.sensors.tracker.Command(control.Motion{Y: 0.2}, t0)
		a.sensors.tracker.Odometry(0.2, 0, t0.Add(20*time.Millisecond))
		a.sensors.tracker.Reset(t0.Add(20 * time.Millisecond))
		for step := 1; step <= 40; step++ {
			a.sensors.tracker.Odometry(motion.DockApproach/0.8, 0, t0.Add(20*time.Millisecond+time.Duration(step*20)*time.Millisecond))
		}
		a.sensors.tracker.Command(control.Motion{}, t0.Add(820*time.Millisecond))
		a.sensors.tracker.Odometry(0, 0, t0.Add(820*time.Millisecond))
		for step := 0; step <= 40; step++ {
			a.sensors.tracker.Gyro(math.Pi/0.8, t0.Add(840*time.Millisecond+time.Duration(step*20)*time.Millisecond))
		}
		if p := a.sensors.tracker.Pose(); len(motion.ApproachPlan(p)) != 0 {
			t.Fatalf("expected empty ApproachPlan at pose %+v", p)
		}
		if err := a.approachDock(context.Background()); err != nil {
			t.Fatalf("approachDock: %v", err)
		}
		snap := a.act.snapshot(a.sensors.tracker.Pose())
		if snap.Last == nil || snap.Last.Kind != "approach" || !snap.Last.Completed || snap.Last.StoppedBy != "" {
			t.Fatalf("Last = %+v, want approach Completed=true StoppedBy=\"\"", snap.Last)
		}
	})

	t.Run("obstacle_handover_marks_completed", func(t *testing.T) {
		a := newTestApp(t)
		now := time.Now()
		a.sensors.setRange(0.10, now.Add(-30*time.Millisecond))
		a.sensors.setRange(0.10, now)

		approachErr := make(chan error, 1)
		nativeStarted := make(chan struct{})
		d := docking.New(a.connContext(), docking.Hooks{
			ShouldApproach: func() bool { return true },
			Approach: func(ctx context.Context) error {
				err := a.approachDock(ctx)
				approachErr <- err
				return err
			},
			Prepare: func(context.Context) error { return nil },
			Start: func(context.Context) error {
				close(nativeStarted)
				return nil
			},
			Cancel: func(context.Context) error { return nil },
			Probe:  func(context.Context) (docking.Power, error) { return docking.Power{}, nil },
		})
		a.setDockManager(d)
		t.Cleanup(func() { _ = d.Cancel(context.Background()) })

		stopGyro := make(chan struct{})
		go func() {
			tk := time.NewTicker(20 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-stopGyro:
					return
				case now := <-tk.C:
					a.sensors.tracker.Gyro(4.0, now)
				}
			}
		}()
		defer close(stopGyro)

		if _, err := a.startDock(context.Background(), 15*time.Second); err != nil {
			t.Fatalf("startDock: %v", err)
		}
		select {
		case err := <-approachErr:
			if err != nil {
				t.Fatalf("approachDock: %v", err)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("approachDock did not finish")
		}
		select {
		case <-nativeStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("native docking did not start after obstacle handover")
		}
		snap := a.act.snapshot(a.sensors.tracker.Pose())
		if snap.Last == nil || snap.Last.Kind != "approach" || !snap.Last.Completed || snap.Last.StoppedBy != "" {
			t.Fatalf("Last = %+v, want approach Completed=true StoppedBy=\"\"", snap.Last)
		}
	})
}

func TestStartDockAsyncAndMutualExclusion(t *testing.T) {
	t.Run("approaching_blocks_move_and_transitions_to_native", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		approachEntered := make(chan struct{})
		releaseApproach := make(chan struct{})
		nativeStarted := make(chan struct{})

		a := &application{
			ctx:        ctx,
			cancel:     cancel,
			connCtx:    ctx,
			connCancel: cancel,
			events:     &events.Log{Dir: t.TempDir()},
		}
		a.ready.Store(true)

		d := docking.New(ctx, docking.Hooks{
			ShouldApproach: func() bool { return true },
			Approach: func(ctx context.Context) error {
				if err := a.driveApproachTick(control.Motion{Y: 0.1}); err != nil {
					return err
				}
				close(approachEntered)
				select {
				case <-releaseApproach:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
			Prepare: func(context.Context) error { return nil },
			Start: func(context.Context) error {
				close(nativeStarted)
				return nil
			},
			Cancel: func(context.Context) error { return nil },
			Probe:  func(context.Context) (docking.Power, error) { return docking.Power{}, nil },
		})
		a.setDockManager(d)
		t.Cleanup(func() { _ = d.Cancel(context.Background()) })

		t0 := time.Now()
		raw, err := a.startDock(ctx, 15*time.Second)
		if err != nil {
			t.Fatalf("startDock: %v", err)
		}
		if time.Since(t0) > 200*time.Millisecond {
			t.Fatalf("startDock blocked for %v; expected async approach return", time.Since(t0))
		}
		op, ok := raw.(docking.Operation)
		if !ok || !op.Active || op.State != "approaching" {
			t.Fatalf("startDock op = %#v, want active approaching", raw)
		}
		if d.NativeActive() {
			t.Fatal("NativeActive reported true during pre-native approach")
		}

		<-approachEntered
		if _, err := a.Move(ctx, motion.Goal{Kind: motion.Drive, Target: 0.5}); err == nil || !strings.Contains(err.Error(), "docking") {
			t.Fatalf("Move during approaching error = %v, want docking error", err)
		}
		if _, err := a.startDock(ctx, 15*time.Second); err == nil {
			t.Fatal("expected second startDock call to fail while active")
		}

		close(releaseApproach)
		select {
		case <-nativeStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("native Start hook did not run after approach completed")
		}
		if !d.NativeActive() {
			t.Fatal("NativeActive reported false after native Start began")
		}
	})

	t.Run("stopAll_cancels_pre_native_without_native_cancel_or_nil_panic", func(t *testing.T) {
		a := newTestApp(t)
		ctx := context.Background()
		inApproach := make(chan struct{})
		var nativeCancels atomic.Int32
		d := docking.New(a.connContext(), docking.Hooks{
			ShouldApproach: func() bool { return true },
			Approach: func(ctx context.Context) error {
				close(inApproach)
				<-ctx.Done()
				return ctx.Err()
			},
			Prepare: func(context.Context) error { return nil },
			Start:   func(context.Context) error { return nil },
			Cancel: func(context.Context) error {
				nativeCancels.Add(1)
				return nil
			},
			Probe: func(context.Context) (docking.Power, error) { return docking.Power{}, nil },
		})
		a.setDockManager(d)

		if _, err := a.startDock(ctx, 15*time.Second); err != nil {
			t.Fatalf("startDock: %v", err)
		}
		<-inApproach
		// stopAll cancels docking cleanly and handles nil a.pub without panicking.
		if err := a.stopAll(ctx); err == nil || !strings.Contains(err.Error(), "not connected") {
			t.Fatalf("stopAll with nil pub = %v, want not connected error", err)
		}
		if d.Active() || nativeCancels.Load() != 0 {
			t.Fatalf("after pre-native stopAll: active=%v cancels=%d", d.Active(), nativeCancels.Load())
		}
	})
}

func TestSyncObjectDetectorAndSetGuardRace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := &application{
		ctx:        ctx,
		cancel:     cancel,
		connCtx:    ctx,
		connCancel: cancel,
		events:     &events.Log{Dir: t.TempDir()},
		settings:   settings{Guard: GuardSettings{Armed: true, Objects: true}},
	}
	a.syncObjectDetector()
	if a.guard.objSub != nil {
		t.Fatal("expected objSub nil while not ready")
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				switch (id + j) % 5 {
				case 0:
					_ = a.setGuard(GuardSettings{Armed: true, Objects: true, Sensitivity: 50, Cooldown: 10})
				case 1:
					_ = a.setGuard(GuardSettings{Armed: false, Objects: false, Sensitivity: 50, Cooldown: 10})
				case 2:
					a.syncObjectDetector()
				case 3:
					a.closeObjectDetector()
				case 4:
					_ = a.status()
					a.ready.Store((j % 2) == 0)
				}
			}
		}(i)
	}
	wg.Wait()
	if err := a.disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
}

func TestDisconnectConcurrentWithStopAllStartDockAndWatchMaster(t *testing.T) {
	for iter := 0; iter < 5; iter++ {
		a := newTestApp(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		a.done = done
		close(done)

		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			a.watchMaster(ctx)
		}()

		var wg sync.WaitGroup
		wg.Add(5)
		go func() { defer wg.Done(); _ = a.disconnect() }()
		go func() { defer wg.Done(); _ = a.stopAll(context.Background()) }()
		go func() { defer wg.Done(); _, _ = a.startDock(context.Background(), 15*time.Second) }()
		go func() { defer wg.Done(); _ = a.cancelDockNative(context.Background()) }()
		go func() { defer wg.Done(); _ = a.publish(control.Motion{}) }()

		wg.Wait()
		cancel()
		<-watchDone
	}
}

func TestRecorderStatusAndSequentialStartFeedStopRace(t *testing.T) {
	binDir := t.TempDir()
	ffmpeg := filepath.Join(binDir, "ffmpeg")
	script := "#!/bin/sh\nout=\"\"\nfor a in \"$@\"; do out=\"$a\"; done\ncat <&3 >/dev/null 2>/dev/null &\nexec cat > \"$out\"\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Run("status_non_blocking_during_audio_wait", func(t *testing.T) {
		media.PutFrame([]byte("jpeg"))
		t.Cleanup(func() { media.PutFrame(nil) })

		var rec recorder
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		outDir := t.TempDir()
		startErr := make(chan error, 1)
		go func() {
			_, err := rec.Start(ctx, outDir)
			startErr <- err
		}()

		deadline := time.Now().Add(time.Second)
		for {
			rec.mu.Lock()
			starting := rec.starting
			rec.mu.Unlock()
			if starting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("recorder did not enter starting phase")
			}
			time.Sleep(5 * time.Millisecond)
		}

		t0 := time.Now()
		st := rec.status()
		if elapsed := time.Since(t0); elapsed > 100*time.Millisecond {
			t.Fatalf("status blocked for %v during audio wait", elapsed)
		}
		if st.Active {
			t.Fatal("status Active = true before ffmpeg started")
		}
		if _, err := rec.Start(ctx, outDir); err == nil || !strings.Contains(err.Error(), "already starting") {
			t.Fatalf("concurrent Start error = %v, want already starting", err)
		}

		cancel()
		select {
		case err := <-startErr:
			if err == nil {
				t.Fatal("expected cancelled error from aborted Start")
			}
		case <-time.After(time.Second):
			t.Fatal("recorder Start did not abort promptly on cancel")
		}
	})

	t.Run("sequential_start_feed_stop_cycles", func(t *testing.T) {
		t.Cleanup(func() { media.PutFrame(nil) })
		var rec recorder
		outDir := t.TempDir()

		for cycle := 0; cycle < 2; cycle++ {
			ctx, cancel := context.WithCancel(context.Background())
			feedDone := make(chan struct{})
			go func() {
				defer close(feedDone)
				tk := time.NewTicker(10 * time.Millisecond)
				defer tk.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tk.C:
						media.PutFrame([]byte("jpeg-frame"))
						media.Audio([]byte{0xff, 0xf1, 0x00})
					}
				}
			}()
			media.PutFrame([]byte("jpeg-frame"))

			path, err := rec.Start(ctx, outDir)
			if err != nil {
				cancel()
				<-feedDone
				t.Fatalf("cycle %d Start: %v", cycle, err)
			}

			deadline := time.Now().Add(2 * time.Second)
			for rec.status().Frames < 2 {
				if time.Now().After(deadline) {
					cancel()
					<-feedDone
					_, _ = rec.Stop()
					t.Fatalf("cycle %d feed did not write frames", cycle)
				}
				time.Sleep(10 * time.Millisecond)
			}

			stoppedPath, err := rec.Stop()
			cancel()
			<-feedDone
			if err != nil {
				t.Fatalf("cycle %d Stop: %v (path=%s)", cycle, err, stoppedPath)
			}
			if stoppedPath != path {
				t.Fatalf("cycle %d Stop path = %q, want %q", cycle, stoppedPath, path)
			}
			if st := rec.status(); st.Active {
				t.Fatalf("cycle %d status still active after Stop", cycle)
			}
			// Different timestamped names per second or ensure next cycle filename differs:
			time.Sleep(1100 * time.Millisecond)
		}
	})
}

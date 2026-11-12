package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/shell-company/scout/internal/control"
	"github.com/shell-company/scout/internal/motion"
)

// activity is what the robot is doing on its own right now: a bounded move or
// a patrol. The HUD and /v1/status show it; Stop cancels it.
type activity struct {
	mu     sync.Mutex
	kind   string // "", "move", "patrol"
	label  string
	mover  *motion.Mover
	route  string
	step   int
	steps  int
	cancel context.CancelFunc
	start  time.Time
	last   *motion.Result
	lastAt time.Time
}

// ActivityStatus is the activity section of /v1/status.
type ActivityStatus struct {
	Kind     string         `json:"kind,omitempty"`
	Label    string         `json:"label,omitempty"`
	Target   float64        `json:"target,omitempty"`
	Achieved float64        `json:"achieved,omitempty"`
	Unit     string         `json:"unit,omitempty"`
	Route    string         `json:"route,omitempty"`
	Step     int            `json:"step,omitempty"`
	Steps    int            `json:"steps,omitempty"`
	Seconds  float64        `json:"seconds,omitempty"`
	Last     *motion.Result `json:"last,omitempty"`
}

var errBusy = errors.New("the robot is already moving on its own; stop it first")

func (ac *activity) begin(kind, label string, cancel context.CancelFunc) error {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.kind != "" {
		return errBusy
	}
	ac.kind, ac.label, ac.cancel, ac.start = kind, label, cancel, time.Now()
	ac.mover, ac.route, ac.step, ac.steps = nil, "", 0, 0
	return nil
}

func (ac *activity) end(r *motion.Result) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.cancel != nil {
		ac.cancel()
	}
	ac.kind, ac.label, ac.mover, ac.cancel = "", "", nil, nil
	if r != nil {
		ac.last, ac.lastAt = r, time.Now()
	}
}

func (ac *activity) setMover(m *motion.Mover) {
	ac.mu.Lock()
	ac.mover = m
	ac.mu.Unlock()
}

// intent returns the motion a running goal stands for (its direction at its
// cruising speed, without ramping or heading corrections), for recording.
func (ac *activity) intent() (control.Motion, bool) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.mover == nil || ac.kind != "move" {
		return control.Motion{}, false
	}
	g := ac.mover.Goal
	v := math.Copysign(g.Speed, g.Target)
	switch g.Kind {
	case motion.Turn:
		return control.Motion{Z: v}, true
	case motion.Strafe:
		return control.Motion{Y: v}, true
	}
	return control.Motion{X: v}, true
}

// stop cancels the current activity, if any.
func (ac *activity) stop() bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.cancel == nil {
		return false
	}
	ac.cancel()
	return true
}

func (ac *activity) snapshot(pose motion.Pose) ActivityStatus {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	st := ActivityStatus{Kind: ac.kind, Label: ac.label, Route: ac.route, Step: ac.step, Steps: ac.steps}
	if ac.kind != "" {
		st.Seconds = time.Since(ac.start).Seconds()
	}
	if ac.mover != nil {
		st.Target, st.Achieved = ac.mover.Goal.Target, ac.mover.Progress(pose)
		st.Unit = "m"
		if ac.mover.Goal.Kind == motion.Turn {
			st.Target, st.Achieved, st.Unit = st.Target*180/math.Pi, st.Achieved*180/math.Pi, "deg"
		}
	}
	if ac.last != nil && time.Since(ac.lastAt) < 8*time.Second {
		st.Last = ac.last
	}
	return st
}

// drive leases one control tick for an autonomous command. It fails if
// someone takes the manual controls or docking starts.
func (a *application) driveTick(m control.Motion) error {
	if !a.ready.Load() {
		return fmt.Errorf("robot not connected")
	}
	return a.dock.WhileIdle(func() error { return a.control.Drive(m, 300*time.Millisecond, time.Now()) })
}

// endLease stops autonomous motion without latching a manual stop.
func (a *application) endLease() {
	_ = a.control.Drive(control.Motion{}, control.Interval, time.Now())
}

// runGoal performs one bounded closed-loop move. It returns when the goal is
// reached or abandoned; the context cancels it.
func (a *application) runGoal(ctx context.Context, g motion.Goal) motion.Result {
	m := motion.NewMover(g, a.sensors.tracker.Pose(), time.Now())
	a.act.setMover(m)
	defer a.endLease()
	t := time.NewTicker(control.Interval)
	defer t.Stop()
	for {
		now := time.Now()
		cmd, done := m.Step(a.sensors.tracker.Pose(), a.sensors.rangeReading(), now)
		if done {
			return m.Result()
		}
		if ctx.Err() != nil {
			return m.Stop("cancelled", nil)
		}
		if err := a.driveTick(cmd); err != nil {
			reason := "error"
			if a.control.CheckAutonomy(now) != nil {
				reason = "manual"
			} else if a.dock != nil && a.dock.Active() {
				reason = "docking"
			}
			return m.Stop(reason, err)
		}
		select {
		case <-ctx.Done():
			a.control.Stop() // drop the lease issued just before a Stop
			return m.Stop("cancelled", nil)
		case <-t.C:
		}
	}
}

// Move runs a single goal as an activity (API, voice, MCP).
func (a *application) Move(parent context.Context, g motion.Goal) (motion.Result, error) {
	if err := g.Validate(); err != nil {
		return motion.Result{}, err
	}
	if !a.ready.Load() {
		return motion.Result{}, fmt.Errorf("robot not connected")
	}
	if err := a.control.CheckAutonomy(time.Now()); err != nil {
		return motion.Result{}, err
	}
	if a.dock != nil && a.dock.Active() {
		return motion.Result{}, fmt.Errorf("docking is in progress")
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(a.connCtx, cancel)
	defer stop()
	label := describeGoal(g)
	if err := a.act.begin("move", label, cancel); err != nil {
		cancel()
		return motion.Result{}, err
	}
	r := a.runGoal(ctx, g)
	a.act.end(&r)
	if r.StoppedBy == "obstacle" {
		a.notices.add(fmt.Sprintf("Stopped %.2f m short: obstacle ahead", math.Abs(g.Target-r.Achieved)), noticeBad)
	}
	return r, nil
}

func describeGoal(g motion.Goal) string {
	switch g.Kind {
	case motion.Turn:
		dir := "left"
		if g.Target < 0 {
			dir = "right"
		}
		return fmt.Sprintf("Turn %s %.0f°", dir, math.Abs(g.Target)*180/math.Pi)
	case motion.Strafe:
		dir := "left"
		if g.Target < 0 {
			dir = "right"
		}
		return fmt.Sprintf("Strafe %s %.2f m", dir, math.Abs(g.Target))
	default:
		dir := "Forward"
		if g.Target < 0 {
			dir = "Back"
		}
		return fmt.Sprintf("%s %.2f m", dir, math.Abs(g.Target))
	}
}

// ---------- Routes (patrols) ----------

type routeStore struct {
	mu        sync.Mutex
	routes    []motion.Route
	loaded    bool
	recording *motion.Recorder
	recName   string
	recAt     time.Time
}

func routesPath() (string, error) {
	dir, err := dataDir("")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "routes.json"), nil
}

func (rs *routeStore) load() {
	if rs.loaded {
		return
	}
	rs.loaded = true
	path, err := routesPath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var list []motion.Route
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("ignoring saved routes: %v", err)
		return
	}
	for _, r := range list {
		if motion.ValidName(r.Name) == nil && len(r.Steps) > 0 && len(r.Steps) <= motion.MaxRouteSteps {
			rs.routes = append(rs.routes, r)
		}
	}
}

func (rs *routeStore) save() error {
	path, err := routesPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(rs.routes, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List returns the saved routes sorted by name.
func (rs *routeStore) List() []motion.Route {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.load()
	out := append([]motion.Route{}, rs.routes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (rs *routeStore) Get(name string) (motion.Route, bool) {
	for _, r := range rs.List() {
		if r.Name == name {
			return r, true
		}
	}
	return motion.Route{}, false
}

func (rs *routeStore) put(r motion.Route) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.load()
	for i := range rs.routes {
		if rs.routes[i].Name == r.Name {
			rs.routes[i] = r
			return rs.save()
		}
	}
	if len(rs.routes) >= 50 {
		return fmt.Errorf("at most 50 routes; delete one first")
	}
	rs.routes = append(rs.routes, r)
	return rs.save()
}

// Delete removes a route.
func (rs *routeStore) Delete(name string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.load()
	for i := range rs.routes {
		if rs.routes[i].Name == name {
			rs.routes = append(rs.routes[:i], rs.routes[i+1:]...)
			return rs.save()
		}
	}
	return fmt.Errorf("no route named %q", name)
}

// Update changes a route's schedule and dock-after setting.
func (rs *routeStore) Update(name string, every int, dockAfter bool) (motion.Route, error) {
	if every < 0 || every > 24*60 || every > 0 && every < 10 {
		return motion.Route{}, fmt.Errorf("schedule must be 0 (off) or 10..1440 minutes")
	}
	r, ok := rs.Get(name)
	if !ok {
		return r, fmt.Errorf("no route named %q", name)
	}
	r.EveryMinutes, r.DockAfter = every, dockAfter
	return r, rs.put(r)
}

// StartRecording begins recording a route from the user's driving.
func (a *application) StartRecording(name string) error {
	if err := motion.ValidName(name); err != nil {
		return err
	}
	rs := &a.routes
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.recording != nil {
		return fmt.Errorf("already recording route %q", rs.recName)
	}
	rs.recording, rs.recName, rs.recAt = &motion.Recorder{}, name, time.Now()
	return nil
}

// recordingRoute reports the route being recorded and its step count.
func (rs *routeStore) recordingRoute() (string, int, time.Duration) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.recording == nil {
		return "", 0, 0
	}
	return rs.recName, rs.recording.Steps(), time.Since(rs.recAt)
}

// sampleRoute feeds the published command into a route being recorded.
func (rs *routeStore) sample(m control.Motion, p motion.Pose, now time.Time) {
	rs.mu.Lock()
	if rs.recording != nil {
		rs.recording.Sample(m, p, now)
	}
	rs.mu.Unlock()
}

// FinishRecording saves (or with discard, drops) the route being recorded.
func (a *application) FinishRecording(discard bool) (motion.Route, error) {
	rs := &a.routes
	rs.mu.Lock()
	rec, name := rs.recording, rs.recName
	rs.recording = nil
	rs.mu.Unlock()
	if rec == nil {
		return motion.Route{}, fmt.Errorf("not recording a route")
	}
	if discard {
		return motion.Route{}, nil
	}
	r, err := rec.Finish(name, a.sensors.tracker.Pose(), time.Now())
	if err != nil {
		return r, err
	}
	if old, ok := rs.Get(name); ok {
		r.EveryMinutes, r.DockAfter = old.EveryMinutes, old.DockAfter
	}
	return r, rs.put(r)
}

// Patrol replays a saved route, then docks if the route asks for it.
func (a *application) Patrol(parent context.Context, name string) error {
	r, ok := a.routes.Get(name)
	if !ok {
		return fmt.Errorf("no route named %q", name)
	}
	if !a.ready.Load() {
		return fmt.Errorf("robot not connected")
	}
	if err := a.control.CheckAutonomy(time.Now()); err != nil {
		return err
	}
	if a.dock != nil && a.dock.Active() {
		return fmt.Errorf("docking is in progress")
	}
	if n, _, _ := a.routes.recordingRoute(); n != "" {
		return fmt.Errorf("finish recording route %q first", n)
	}
	ctx, cancel := context.WithCancel(a.connCtx)
	if err := a.act.begin("patrol", "Patrol: "+r.Name, cancel); err != nil {
		cancel()
		return err
	}
	a.act.mu.Lock()
	a.act.route, a.act.steps = r.Name, len(r.Steps)
	a.act.mu.Unlock()
	r.LastRun = time.Now()
	_ = a.routes.put(r)
	a.logEvent("patrol", "Patrol started: "+r.Name, false)
	go func() {
		res := a.runRoute(ctx, r)
		a.act.end(&res)
		if res.Completed {
			a.logEvent("patrol", "Patrol finished: "+r.Name, true)
			a.notices.add("Patrol finished: "+r.Name, noticeGood)
			if r.DockAfter && a.dock != nil {
				if _, err := a.startDock(a.connCtx, 300*time.Second); err != nil {
					a.notices.add("Could not dock after the patrol: "+err.Error(), noticeBad)
				}
			}
			return
		}
		msg := fmt.Sprintf("Patrol %s stopped (%s) at step %d of %d", r.Name, res.StoppedBy, int(res.Achieved)+1, len(r.Steps))
		a.logEvent("patrol", msg, true)
		a.notices.add(msg, noticeBad)
		// Blocked or stuck: go home rather than wait off the charger. A person
		// who stopped or took over the patrol keeps the robot where it is.
		switch res.StoppedBy {
		case "obstacle", "stalled", "timeout":
			if r.DockAfter && a.dock != nil {
				if _, err := a.startDock(a.connCtx, 300*time.Second); err != nil {
					a.notices.add("Could not dock after the patrol: "+err.Error(), noticeBad)
				}
			}
		}
	}()
	return nil
}

func (a *application) runRoute(ctx context.Context, r motion.Route) motion.Result {
	odometry := a.sensors.tracker.OdometryValid()
	total := motion.Result{Kind: "patrol", Target: float64(len(r.Steps))}
	started := time.Now()
	for i, s := range r.Steps {
		a.act.mu.Lock()
		a.act.step = i + 1
		a.act.mu.Unlock()
		goal, m, secs := s.Plan(odometry)
		if goal != nil {
			res := a.runGoal(ctx, *goal)
			a.act.setMover(nil)
			if res.StoppedBy == "obstacle" {
				// Someone may be passing: wait, then finish the step once.
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
					rest := *goal
					rest.Target -= res.Achieved
					if math.Abs(rest.Target) > 0.03 {
						res = a.runGoal(ctx, rest)
						a.act.setMover(nil)
					} else {
						res.StoppedBy, res.Completed = "", true
					}
				}
			}
			if !res.Completed && res.StoppedBy != "timeout" && res.StoppedBy != "stalled" {
				total.StoppedBy, total.Error = res.StoppedBy, res.Error
				total.Achieved, total.Seconds = float64(i), time.Since(started).Seconds()
				return total
			}
			continue
		}
		if reason, err := a.runTimed(ctx, m, time.Duration(secs*float64(time.Second))); reason != "" {
			total.StoppedBy, total.Achieved = reason, float64(i)
			if err != nil {
				total.Error = err.Error()
			}
			total.Seconds = time.Since(started).Seconds()
			return total
		}
	}
	total.Completed, total.Achieved, total.Seconds = true, float64(len(r.Steps)), time.Since(started).Seconds()
	return total
}

// runTimed holds a command for a duration (arcs and idle pauses in a route).
func (a *application) runTimed(ctx context.Context, m control.Motion, d time.Duration) (string, error) {
	defer a.endLease()
	end := time.Now().Add(d)
	t := time.NewTicker(control.Interval)
	defer t.Stop()
	for time.Now().Before(end) {
		if m != (control.Motion{}) {
			if err := a.driveTick(m); err != nil {
				return "manual", err
			}
		} else if err := a.control.CheckAutonomy(time.Now()); err != nil {
			return "manual", err
		}
		select {
		case <-ctx.Done():
			return "cancelled", nil
		case <-t.C:
		}
	}
	return "", nil
}

// watchPatrols runs scheduled patrols. A patrol starts only when the robot is
// on the charger with at least 50% battery, nobody has driven for two
// minutes, and nothing else is moving the robot.
func (a *application) watchPatrols(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	var lastManual time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if o := a.control.Snapshot().Owner; o == "manual" || o == "manual-stop" || o == "agent" {
			lastManual = time.Now()
		}
		batteryMu.RLock()
		pct, charge, at := batteryPercent, batteryChargeState, batteryAt
		batteryMu.RUnlock()
		if time.Since(at) > batteryFreshFor || pct < 50 || (charge != 0 && charge != 2) || time.Since(lastManual) < 2*time.Minute {
			continue
		}
		if a.dock != nil && a.dock.Active() || a.act.snapshot(motion.Pose{}).Kind != "" {
			continue
		}
		for _, r := range a.routes.List() {
			if r.EveryMinutes == 0 || time.Since(r.LastRun) < time.Duration(r.EveryMinutes)*time.Minute {
				continue
			}
			log.Printf("scheduled patrol: %s", r.Name)
			if err := a.Patrol(ctx, r.Name); err != nil {
				log.Printf("scheduled patrol %s: %v", r.Name, err)
				r.LastRun = time.Now() // try again next interval, not every 20 s
				_ = a.routes.put(r)
			}
			break
		}
	}
}

// ---------- Assisted docking ----------

// The firmware finds the dock by its QR code, which it can miss when the
// robot is close, off to the side or facing away. When the map origin is the
// dock (the app connected while charging, or after a verified docking),
// the app first drives to a point in front of the dock, facing it, then
// hands over to the firmware's search and reverse.

func (a *application) setOriginAtDock(v bool) {
	a.sensors.mu.Lock()
	a.sensors.originAtDock = v
	a.sensors.mu.Unlock()
}

func (a *application) originAtDock() bool {
	a.sensors.mu.Lock()
	defer a.sensors.mu.Unlock()
	return a.sensors.originAtDock
}

// needsApproach reports whether assisted docking should drive first.
func (a *application) needsApproach() bool {
	if !a.originAtDock() || !a.sensors.tracker.OdometryValid() || a.audio == nil {
		return false // docking itself needs SSH; don't drive first and then fail
	}
	// Already on the dock: docking only has to confirm charging.
	batteryMu.RLock()
	charge, at := batteryChargeState, batteryAt
	batteryMu.RUnlock()
	if time.Since(at) < 15*time.Second && (charge == 0 || charge == 2) {
		return false
	}
	p := a.sensors.tracker.Pose()
	if math.Hypot(p.X, p.Y) < 0.1 {
		return false
	}
	far := math.Hypot(p.X-motion.DockApproach, p.Y)
	if far > 6 {
		return false // too much dead-reckoning drift to be useful
	}
	return far > 0.15 || math.Abs(math.Remainder(p.Yaw-math.Pi, 2*math.Pi)) > 20*math.Pi/180
}

// approachDock drives to the approach point, planning again from the
// measured pose after every turn or drive. It returns early (without an
// error) at an obstacle, after turning to face the dock, so the firmware can
// take over from there.
func (a *application) approachDock(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(a.connCtx, cancel)
	defer stop()
	if err := a.act.begin("move", "Approaching the dock", cancel); err != nil {
		cancel()
		return err
	}
	var last motion.Result
	defer func() { last.Kind = "approach"; a.act.end(&last) }()
	misses := 0
	for i := 0; i < 12; i++ {
		plan := motion.ApproachPlan(a.sensors.tracker.Pose())
		if len(plan) == 0 {
			return nil
		}
		last = a.runGoal(ctx, plan[0])
		a.act.setMover(nil)
		switch last.StoppedBy {
		case "":
		case "timeout", "stalled":
			if misses++; misses >= 2 {
				return fmt.Errorf("approach to the dock %s twice", last.StoppedBy)
			}
		case "obstacle":
			p := a.sensors.tracker.Pose()
			if t := math.Remainder(math.Pi-p.Yaw, 2*math.Pi); math.Abs(t) > 0.1 {
				last = a.runGoal(ctx, motion.Goal{Kind: motion.Turn, Target: t, Speed: 1})
			}
			return nil
		default:
			return fmt.Errorf("approach to the dock stopped: %s", last.StoppedBy)
		}
	}
	return nil
}

// startDock begins docking, driving to the dock's approach point first when
// that helps. It blocks while approaching (up to about a minute).
func (a *application) startDock(ctx context.Context, timeout time.Duration) (any, error) {
	if a.dock == nil {
		return nil, fmt.Errorf("robot not connected")
	}
	if a.needsApproach() {
		if err := a.control.CheckAutonomy(time.Now()); err != nil {
			return nil, err
		}
		if a.act.snapshot(motion.Pose{}).Kind != "" {
			return nil, errBusy
		}
		log.Printf("assisted docking: approaching the dock first")
		if err := a.approachDock(ctx); err != nil {
			return nil, err
		}
	}
	a.unsubscribeOdometry() // see subscribeOdometry
	op, err := a.dock.Start(timeout)
	if err != nil {
		a.subscribeOdometry()
	}
	return op, err
}

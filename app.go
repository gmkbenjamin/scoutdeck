package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msgs/geometry_msgs"
	"github.com/aler9/goroslib/pkg/msgs/std_msgs"
	"github.com/hajimehoshi/ebiten/v2"
	"github.com/gmkbenjamin/scoutdeck/internal/agent"
	"github.com/gmkbenjamin/scoutdeck/internal/control"
	"github.com/gmkbenjamin/scoutdeck/internal/docking"
	"github.com/gmkbenjamin/scoutdeck/internal/events"
	"github.com/gmkbenjamin/scoutdeck/internal/motion"
	"github.com/gmkbenjamin/scoutdeck/internal/web"
)

var flagAPI = flag.String("api", "127.0.0.1:8765", "loopback address for the agent API and web app, or \"off\" (token: SCOUTDECK_API_TOKEN or the saved api-token)")
var flagInputTest = flag.Bool("input-test", false, "test keyboard/gamepad input in a window without connecting to a robot")
var flagHeadless = flag.Bool("headless", false, "run without a window; requires -api")
var flagSSH = flag.String("ssh", "", "SSH destination; default root@<host> using your SSH keys/agent (-no-ssh disables)")

// Connection phases shown by the HUD.
const (
	connSetup = iota
	connConnecting
	connFailed
	connReady
)

type application struct {
	ctx          context.Context
	cancel       context.CancelFunc
	control      control.Controller
	actions      *control.Actions
	serviceMu    sync.Mutex
	pub          *goroslib.Publisher
	cancelDetect *goroslib.Publisher
	dockMu       sync.RWMutex
	dock         *docking.Manager
	opMu         sync.Mutex
	opWG         sync.WaitGroup
	motionMu     sync.Mutex

	settings   settings
	target     target
	connCtx    context.Context // cancelled when this robot connection ends
	connCancel context.CancelFunc
	lostOnce   *sync.Once
	audio      *agent.SSHAudio
	subs       []*goroslib.Subscriber
	server     *http.Server
	done       chan struct{} // closed when the velocity loop exits
	ctrlErr    error

	// ready is set only after every field above is initialised, so the UI and
	// API never observe a half-connected application.
	ready      atomic.Bool
	humanAt    atomic.Int64 // last manual input or Stop (unix nanos)
	connecting sync.WaitGroup
	connMu     sync.Mutex
	connPhase  int
	connErr    string
	connNote   string // guidance shown on the connect screen, e.g. after a reset
	apiToken   string
	voice      voiceAssistant
	settingsMu sync.Mutex
	autoDock   autoDocker
	phones     phoneAddresses
	codes      web.OneTimeCodes
	suggest    string // address to prefill on the connect screen
	notices    notices

	sensors     sensors
	act         activity
	routes      routeStore
	guard       guard
	events      *events.Log
	rec         recorder
	cameraCache cameraCache
	battery     batteryHistory
	odomMu      sync.Mutex
	odomSub     *goroslib.Subscriber
}

func (a *application) dockManager() *docking.Manager {
	a.dockMu.RLock()
	defer a.dockMu.RUnlock()
	return a.dock
}

func (a *application) setDockManager(d *docking.Manager) {
	a.dockMu.Lock()
	a.dock = d
	a.dockMu.Unlock()
}

func (a *application) dockingActive() bool {
	d := a.dockManager()
	return d != nil && d.Active()
}

func (a *application) dockingNative() bool {
	d := a.dockManager()
	return d != nil && d.NativeActive()
}

func (a *application) connContext() context.Context {
	a.connMu.Lock()
	ctx := a.connCtx
	a.connMu.Unlock()
	if ctx != nil {
		return ctx
	}
	return a.ctx
}

func (a *application) connState() (int, string, target) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.connPhase, a.connErr, a.target
}

func (a *application) note() string {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.connNote
}

func (a *application) suggestHost(host string) {
	a.connMu.Lock()
	a.suggest = host
	a.connMu.Unlock()
}

func (a *application) takeSuggestion() string {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	s := a.suggest
	a.suggest = ""
	return s
}

// lost tears down a connection that ended without the user quitting and
// returns the window to the connect screen. Safe to call more than once.
func (a *application) lost(reason, note string) {
	once := a.lostOnce
	if once == nil {
		return
	}
	once.Do(func() {
		if *flagHeadless {
			log.Printf("connection lost: %s", reason)
			a.cancel()
			return
		}
		a.connecting.Add(1)
		go func() {
			defer a.connecting.Done()
			if err := a.disconnect(); err != nil {
				log.Printf("disconnect: %v", err)
			}
			a.connMu.Lock()
			a.connPhase, a.connErr, a.connNote = connFailed, reason, note
			a.connMu.Unlock()
		}()
	})
}

// watchMaster ends the connection when the ROS master stops answering, e.g.
// because the robot rebooted or changed networks.
func (a *application) watchMaster(ctx context.Context) {
	failures := 0
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		node := rosNode()
		if node == nil {
			return
		}
		res := make(chan error, 1)
		go func() { _, err := node.MasterGetTopics(); res <- err }()
		var err error
		select {
		case err = <-res:
		case <-time.After(4 * time.Second):
			err = fmt.Errorf("timed out")
		case <-ctx.Done():
			return
		}
		if err == nil {
			failures = 0
			continue
		}
		if failures++; failures >= 3 {
			a.lost("Lost contact with the robot: "+err.Error(), "")
			return
		}
	}
}

func (a *application) setConn(phase int, err string) {
	a.connMu.Lock()
	a.connPhase, a.connErr = phase, err
	a.connMu.Unlock()
}

func runApplication() (retErr error) {
	switch *flagControlScheme {
	case "auto", "keyboard", "gamepad", "joystick":
	default:
		return fmt.Errorf("-c must be auto, keyboard, gamepad or joystick")
	}
	if *flagInputTest && *flagHeadless {
		return fmt.Errorf("-input-test needs a window; omit -headless")
	}
	if *flagAPI == "off" {
		*flagAPI = ""
	}
	if *flagHeadless && *flagAPI == "" {
		return fmt.Errorf("-headless requires -api")
	}
	if *flagStream != "jpg" && *flagStream != "h264" {
		return fmt.Errorf("-stream must be jpg or h264")
	}
	saved := loadSettings()
	t, err := resolveTarget(saved)
	if err != nil {
		return err
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(ctx)
	a := &application{ctx: ctx, cancel: cancel, actions: control.NewActions(), settings: saved, target: t, events: &events.Log{Max: 300}}
	if dir, err := dataDir("events"); err == nil {
		a.events.Dir = dir
	} else {
		log.Printf("events are not saved: %v", err)
	}
	defer func() {
		cancel()
		a.connecting.Wait()
		if err := a.disconnect(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	if *flagInputTest {
		return runInputTest(a)
	}
	if *flagAPI != "" {
		if err := a.startAPI(); err != nil {
			if *flagHeadless {
				return err
			}
			log.Printf("web app and agent API unavailable: %v", err)
			a.notices.add("Web app unavailable: "+err.Error(), noticeBad)
		}
	}
	defer func() {
		if a.server != nil {
			_ = a.server.Close()
		}
	}()
	go a.actions.Run(ctx, func(err error) {
		if err != nil {
			log.Printf("action failed: %v", err)
			a.notices.add(err.Error(), noticeBad)
		}
	})
	if *flagHeadless {
		if t.Host == "" {
			return fmt.Errorf("no robot address: pass -host <ip> once and it will be remembered")
		}
		if err := a.connect(t); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	}
	if t.Host != "" {
		a.startConnect(t)
	}
	return runWindow(a, false)
}

// startConnect connects in the background so the window stays responsive.
func (a *application) startConnect(t target) {
	a.connMu.Lock()
	a.target = t
	a.connPhase, a.connErr, a.connNote = connConnecting, "", ""
	a.connMu.Unlock()
	a.connecting.Add(1)
	go func() {
		defer a.connecting.Done()
		if err := a.connect(t); err != nil {
			log.Printf("connect to %s failed: %v", t.Host, err)
			if derr := a.disconnect(); derr != nil {
				log.Printf("cleanup after failed connect: %v", derr)
			}
			a.setConn(connFailed, err.Error())
		}
	}()
}

func (a *application) connect(t target) error {
	if a.ctx.Err() != nil {
		return a.ctx.Err()
	}
	*flagROSHostAddress = t.ROS
	*flagSSH = t.SSH
	connCtx, connCancel := context.WithCancel(a.ctx)
	a.connMu.Lock()
	a.connCtx, a.connCancel = connCtx, connCancel
	a.connMu.Unlock()
	a.lostOnce = &sync.Once{}
	a.audio = nil
	if t.SSH != "" {
		audio, err := agent.NewSSHAudio(t.SSH)
		if err != nil {
			return err
		}
		a.audio = audio
	}
	if err := forceROSHostnameToMasterIP(t.ROS, *flagVerbose); err != nil {
		return err
	}
	node, err := goroslib.NewNode(goroslib.NodeConf{Name: fmt.Sprintf("scoutdeck_%d", os.Getpid()), MasterAddress: t.ROS, Host: resolveNodeHost(t.ROS, *flaglocalhost)})
	if err != nil {
		return fmt.Errorf("cannot reach the robot's ROS master at %s: %w", t.ROS, err)
	}
	setROSNode(node)
	pub, err := goroslib.NewPublisher(goroslib.PublisherConf{Node: node, Topic: "/cmd_vel", Msg: &geometry_msgs.Twist{}})
	if err != nil {
		return err
	}
	cancelDetect, err := goroslib.NewPublisher(goroslib.PublisherConf{Node: node, Topic: "/AppNode/cancel_detect", Msg: &std_msgs.Int32{}})
	if err != nil {
		_ = pub.Close()
		return err
	}
	a.motionMu.Lock()
	a.pub, a.cancelDetect = pub, cancelDetect
	a.motionMu.Unlock()
	hooks := docking.Hooks{
		Prepare: func(ctx context.Context) error {
			if err := a.control.CheckAutonomy(time.Now()); err != nil {
				return err
			}
			a.unsubscribeOdometry()
			a.setOriginAtDock(false)
			a.control.Stop()
			a.motionMu.Lock()
			defer a.motionMu.Unlock()
			return a.publishRaw(control.Motion{})
		},
		ShouldApproach: a.needsApproach,
		Approach:       a.approachDock,
		Start:          scoutGoHome,
		Cancel:         a.cancelDockNative,
	}
	if a.audio != nil {
		hooks.Probe = a.audio.Power
	}
	a.setDockManager(docking.New(connCtx, hooks))
	if *flagStream == "h264" {
		dec, err := newH264Decoder()
		if err != nil {
			return err
		}
		h264DecPtr.Store(dec)
		if *flagDumpH264 != "" {
			f, err := os.Create(*flagDumpH264)
			if err != nil {
				return err
			}
			h264DumpMu.Lock()
			h264DumpFile = f
			h264DumpMu.Unlock()
		}
	}
	a.sensors.night = -1
	a.sensors.tracker.Reset(time.Now())
	for _, conf := range append([]goroslib.SubscriberConf{
		{Node: node, Topic: "/CoreNode/going_home_status", Callback: func(s *std_msgs.Int32) {
			if d := a.dockManager(); d != nil {
				d.Phase(s.Data)
			}
		}, QueueSize: 8},
		{Node: node, Topic: streamTopicFromFlag(*flagStream), Callback: onMessageFrame},
		{Node: node, Topic: "/SensorNode/simple_battery_status", Callback: onBatteryStatus, QueueSize: 1},
		{Node: node, Topic: "/CoreNode/aac", Callback: func(f *Frame) {
			if f.Type == Frame_AUDIO_STREAM_AAC {
				media.Audio(f.Data)
			}
		}, QueueSize: 8},
	}, a.sensorSubscriptions(node)...) {
		s, err := goroslib.NewSubscriber(conf)
		if err != nil {
			return err
		}
		a.subs = append(a.subs, s)
	}
	a.done = make(chan struct{})
	done := a.done
	go func() {
		t := time.NewTicker(control.Interval)
		err := a.control.Run(connCtx, t.C, a.publish)
		t.Stop()
		a.ctrlErr = err
		close(done)
		if err != nil {
			a.lost("Lost contact with the robot: "+err.Error(), "")
		}
	}()
	go a.watchMaster(connCtx)
	log.Printf("ScoutDeck connected to ROS master %s; video=%s", t.ROS, *flagStream)
	if *flagVoice && a.voice.State() == "" {
		defer a.toggleVoice()
	}
	a.setConn(connReady, "")
	a.ready.Store(true)
	go a.watchAutoDock(connCtx)
	go a.watchGuard(connCtx)
	go a.watchPatrols(connCtx)
	go a.watchRobot(connCtx)
	go a.watchPower(connCtx)
	a.subscribeOdometry()
	go a.watchDockOrigin(connCtx)
	a.syncObjectDetector()
	a.settingsMu.Lock()
	changed := a.settings.Host != t.Host
	a.settings.Host = t.Host
	a.settingsMu.Unlock()
	if changed {
		if err := a.saveSettings(); err != nil {
			log.Printf("could not save robot address: %v", err)
		}
	}
	return nil
}

// startAPI serves the agent API and the web app for the whole session; it
// answers "robot not connected" while there is no robot connection.
func (a *application) startAPI() error {
	host, _, err := net.SplitHostPort(*flagAPI)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-api must bind a literal loopback IP; use tailscale serve or an SSH tunnel for remote access")
	}
	token, where, err := apiToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *flagAPI)
	if err != nil {
		return err
	}
	a.apiToken = token
	api := &agent.API{Token: token, Control: &a.control, Media: media, Status: a.status, Action: a.action, Shutdown: a.cancel}
	api.StartDock = func(seconds int) (any, error) { return a.startDock(a.connContext(), time.Duration(seconds)*time.Second) }
	api.DockStatus = func() any {
		if d := a.dockManager(); d != nil {
			return d.Snapshot()
		}
		return docking.Operation{State: "idle"}
	}
	api.Stop = a.stopAll
	api.Drive = func(m control.Motion, ttl time.Duration) error {
		d := a.dockManager()
		if d == nil {
			return fmt.Errorf("robot not connected")
		}
		return d.WhileIdle(func() error { return a.control.Drive(m, ttl, time.Now()) })
	}
	api.Record = (&agent.Microphone{Media: media}).Record
	api.Play = func(ctx context.Context, wav []byte) error {
		if a.audio == nil {
			return fmt.Errorf("SSH audio is not configured")
		}
		return a.audio.Play(ctx, wav)
	}
	api.Power = func(ctx context.Context) (any, error) {
		if a.audio == nil {
			return nil, fmt.Errorf("live charging checks require SSH")
		}
		return a.audio.Power(ctx)
	}
	api.AutoDock = func() any { return a.autoDockSettings() }
	api.SetAutoDock = func(enabled bool, threshold int) (any, error) {
		s := AutoDockSettings{Enabled: enabled, Threshold: threshold}
		if err := a.setAutoDock(s); err != nil {
			return nil, err
		}
		return s, nil
	}
	api.Extra = a.registerFeatures
	robotOnly := api.Handler()
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.ready.Load() && !offlineOK(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"robot not connected"}`)
			return
		}
		robotOnly.ServeHTTP(w, r)
	})
	a.server = &http.Server{Handler: web.Handler(token, &a.codes, guarded), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return a.ctx }}
	server := a.server
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("agent API: %v", err)
		}
	}()
	log.Printf("Agent API and web app at http://%s (token: %s)", listener.Addr(), where)
	if *flagLAN {
		a.settingsMu.Lock()
		a.settings.LANAccess = true
		a.settingsMu.Unlock()
		if err := a.saveSettings(); err != nil {
			log.Printf("could not save home Wi-Fi access: %v", err)
		}
	}
	go a.watchPhoneAddresses()
	return nil
}

// disconnect releases whatever connect created; it is safe after a partial
// connect and returns the velocity loop's error, if any.
func (a *application) disconnect() error {
	var retErr error
	a.ready.Store(false)
	a.connMu.Lock()
	connCancel := a.connCancel
	a.connCancel = nil
	a.connMu.Unlock()
	if connCancel != nil {
		connCancel()
	}
	// Wait for any in-flight Move/Patrol/startDock entering under opMu to
	// observe !ready / cancelled connCtx before tearing down docking or ROS.
	a.opMu.Lock()
	a.opMu.Unlock()
	a.act.stop()
	a.opWG.Wait()
	a.closeObjectDetector()
	a.unsubscribeOdometry()
	if a.rec.status().Active {
		if path, err := a.rec.Stop(); err == nil {
			a.notices.add("Recording saved: "+path, noticeGood)
		}
	}
	if d := a.dockManager(); d != nil && d.Active() {
		cleanup, stop := context.WithTimeout(context.Background(), 9*time.Second)
		if err := d.Cancel(cleanup); err != nil {
			log.Printf("docking cancellation failed: %v", err)
			retErr = err
		}
		stop()
	}
	if a.done != nil {
		<-a.done
		if retErr == nil {
			retErr = a.ctrlErr
		}
		a.done = nil
	}
	// Unblock any camera callback writing to ffmpeg before waiting on subscribers.
	if dec := h264DecPtr.Swap(nil); dec != nil {
		dec.Close()
	}
	for _, s := range a.subs {
		_ = s.Close()
	}
	a.subs = nil
	h264DumpMu.Lock()
	if h264DumpFile != nil {
		_ = h264DumpFile.Close()
		h264DumpFile = nil
	}
	h264DumpMu.Unlock()
	a.motionMu.Lock()
	pub, cancelDetect := a.pub, a.cancelDetect
	a.pub, a.cancelDetect = nil, nil
	a.motionMu.Unlock()
	if pub != nil {
		_ = pub.Close()
	}
	if cancelDetect != nil {
		_ = cancelDetect.Close()
	}
	nodeMu.Lock()
	node := n
	n = nil
	nodeMu.Unlock()
	if node != nil {
		_ = node.Close()
	}
	a.setDockManager(nil)
	return retErr
}

func (a *application) publish(m control.Motion) error {
	a.motionMu.Lock()
	defer a.motionMu.Unlock()
	if o := a.control.Sending(); o == "manual" || o == "manual-stop" {
		a.humanAt.Store(time.Now().UnixNano())
	}
	d := a.dockManager()
	nativeDocking := d != nil && d.NativeActive()
	a.sensors.tracker.External(nativeDocking)
	if nativeDocking {
		if m != (control.Motion{}) {
			a.control.Stop()
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
				defer cancel()
				if err := d.Cancel(ctx); err != nil {
					log.Printf("manual docking override failed: %v", err)
				}
			}()
		}
		return nil
	}
	now := time.Now()
	if a.guardForward() {
		if limited, ok := motion.Limit(m, a.sensors.rangeReading(), now); ok {
			m = limited
			a.sensors.setLimited(now)
		}
	}
	a.sensors.tracker.Command(m, now)
	rec := m
	if intent, ok := a.act.intent(); ok && m != (control.Motion{}) {
		rec = intent
	}
	a.routes.sample(rec, a.sensors.tracker.Pose(), now)
	return a.publishRaw(m)
}

// guardForward reports whether forward motion is limited near obstacles:
// always for autonomous motion, and for manual driving unless turned off.
func (a *application) guardForward() bool {
	if a.control.Sending() == "agent" {
		return true
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	return !a.settings.ObstacleGuardOff
}

func (a *application) writeCancelDetect(ctx context.Context) error {
	a.motionMu.Lock()
	pub := a.cancelDetect
	a.motionMu.Unlock()
	if pub == nil {
		return fmt.Errorf("not connected to the robot")
	}
	return pub.WriteSync(ctx, &std_msgs.Int32{Data: 1})
}

func (a *application) publishRaw(m control.Motion) error {
	if a.pub == nil {
		return fmt.Errorf("not connected to the robot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if *flagVerbose {
		log.Printf("cmd_vel: x=%.3f y=%.3f z=%.3f", m.X, m.Y, m.Z)
	}
	return a.pub.WriteSync(ctx, &geometry_msgs.Twist{Linear: geometry_msgs.Vector3{X: m.X, Y: m.Y}, Angular: geometry_msgs.Vector3{Z: m.Z}})
}

func (a *application) action(parent context.Context, name string, value int) error {
	if !a.serviceMu.TryLock() {
		return fmt.Errorf("another robot service call is running")
	}
	defer a.serviceMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	stop := context.AfterFunc(a.ctx, cancel)
	defer stop()
	switch name {
	case "home":
		_, err := a.startDock(a.connContext(), 300*time.Second)
		return err
	case "light":
		return turnOnLight(ctx, int32(value))
	default:
		return fmt.Errorf("unknown action %q", name)
	}
}

func (a *application) status() any {
	batteryMu.RLock()
	battery := map[string]any{"percent": batteryPercent, "charge_state": batteryChargeState, "observed_at": batteryAt}
	batteryMu.RUnlock()
	f := media.Frame()
	var dock any
	ctrl := a.control.Snapshot()
	if d := a.dockManager(); d != nil {
		dock = d.Snapshot()
		if d.Active() {
			ctrl.Owner = "docking"
		}
	}
	phase, connErr, t := a.connState()
	robot := map[string]any{"connected": a.ready.Load(), "host": t.Host, "ssh": t.SSH != ""}
	if !a.ready.Load() {
		robot["message"] = "The ScoutDeck app on the Mac isn't connected to the robot."
		if phase == connConnecting {
			robot["message"] = "The Mac is connecting to the robot…"
		} else if connErr != "" {
			robot["message"] = "Robot offline: " + connErr
		}
	}
	ad := a.autoDockSettings()
	autoDock := map[string]any{"enabled": ad.Enabled, "threshold": ad.Threshold, "countdown_seconds": a.autoDockCountdown()}
	now := time.Now()
	g := a.guardSettings()
	guard := map[string]any{"armed": g.Armed, "sensitivity": g.Sensitivity, "notify": g.Notify, "objects": g.Objects, "events_today": len(a.events.List(0, now.Truncate(24*time.Hour)))}
	if last := a.events.List(1, time.Time{}); len(last) > 0 {
		guard["last_event"] = last[0]
	}
	routeName, routeSteps, routeFor := a.routes.recordingRoute()
	routeRec := map[string]any{"active": routeName != ""}
	if routeName != "" {
		routeRec["name"], routeRec["steps"], routeRec["seconds"] = routeName, routeSteps, routeFor.Seconds()
	}
	var detections []map[string]any
	for _, d := range a.sensors.detections(now) {
		detections = append(detections, map[string]any{"name": d.Name, "score": d.Score, "box": d.Box})
	}
	if b := a.motionBox(now); b != nil {
		detections = append(detections, map[string]any{"name": "motion", "box": [4]float64{b.Left, b.Top, b.Right, b.Bottom}})
	}
	return map[string]any{"robot": robot, "auto_dock": autoDock, "control": ctrl, "docking": dock, "battery": battery,
		"camera":    map[string]any{"available": len(f.Data) > 0 && time.Since(f.At) < 3*time.Second, "observed_at": f.At},
		"sensors":   a.sensorStatus(now),
		"activity":  a.act.snapshot(a.sensors.tracker.Pose()),
		"recording": a.rec.status(), "guard": guard, "route_recording": routeRec, "detections": detections,
		"capabilities": map[string]any{"drive": true, "move": true, "turn": true, "screenshot": true, "video": "mjpeg", "microphone": "aac", "ssh_audio": *flagSSH != "", "light": true, "dock": *flagSSH != "",
			"record": true, "routes": true, "guard": true, "camera_settings": true, "say": *flagSSH != ""},
		"limits": map[string]any{"linear_x": .2, "linear_y": .3, "angular_z": 1.8, "max_ttl_ms": 500, "move_m": motion.MaxDistance, "turn_deg": 720}}
}

// watchDockOrigin moves the map origin to the dock after each verified docking.
func (a *application) watchDockOrigin(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last, retried, crashed := "", false, ""
	var lastCancel time.Time
	cancelTries := 0
	if d := a.dockManager(); d != nil {
		last = d.Snapshot().ID
	}
	decided, started := false, time.Now()
	a.setOriginAtDock(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d := a.dockManager()
		if d == nil {
			continue
		}
		// The map origin is the dock if the robot was charging when tracking started.
		if !decided {
			batteryMu.RLock()
			charge, at := batteryChargeState, batteryAt
			batteryMu.RUnlock()
			switch {
			case at.After(started):
				decided = true
				if (charge == 0 || charge == 2) && a.sensors.tracker.Pose().Odometer < 0.05 {
					a.setOriginAtDock(true)
				}
			case time.Since(started) > 15*time.Second:
				decided = true // no fresh reading: don't guess
			}
		}
		op := d.Snapshot()
		if op.Active && op.State != "approaching" {
			// See subscribeOdometry. Positions are unknown while the firmware
			// drives, so the map can't locate the dock until docking succeeds.
			a.unsubscribeOdometry()
			a.setOriginAtDock(false)
		} else if !op.Active {
			a.subscribeOdometry()
		}
		// The firmware's camera node can crash mid-docking and come back a
		// few seconds later; until then the cancel call is refused. Keep
		// retrying the cancel so the robot does not stay locked.
		if op.Active && op.State == "cancel_failed" && time.Since(lastCancel) >= 5*time.Second && cancelTries < 12 {
			lastCancel, cancelTries = time.Now(), cancelTries+1
			call, done := context.WithTimeout(ctx, 8*time.Second)
			if err := d.Cancel(call); err == nil {
				log.Printf("docking: cancellation confirmed after a firmware restart")
				crashed = op.ID
			}
			done()
			continue
		}
		if !op.Active || op.State != "cancel_failed" {
			cancelTries = 0
		}
		if op.ID == "" || op.ID == last || op.Active {
			continue
		}
		last = op.ID
		switch op.State {
		case "charging":
			retried = false
			if a.act.snapshot(motion.Pose{}).Kind == "" {
				a.sensors.tracker.Reset(time.Now())
			}
			a.setOriginAtDock(true)
			a.logEvent("dock", "Docked and charging", false)
		case "failed", "timed_out", "cancelled":
			if op.State == "cancelled" && crashed != op.ID {
				break // a person or the API cancelled it
			}
			msg := "Docking " + strings.ReplaceAll(op.State, "_", " ")
			if op.Error != "" {
				msg += ": " + op.Error
			}
			a.logEvent("dock", msg, true)
			// One retry per failure streak (a success re-arms it), only
			// when the firmware itself gave up or ran out of time, and never
			// after someone drove or pressed Stop during the attempt.
			firmware := op.State == "timed_out" || op.FirmwarePhase == 5 || op.FirmwarePhase == 6 || crashed == op.ID
			human := time.Unix(0, a.humanAt.Load()).After(op.StartedAt)
			if !retried && firmware && !human && a.audio != nil {
				retried = true
				a.notices.add("Docking failed - searching for the dock again", noticeInfo)
				go func(failedAt time.Time) {
					time.Sleep(1500 * time.Millisecond)
					if time.Unix(0, a.humanAt.Load()).After(failedAt) {
						return // someone took over meanwhile
					}
					if _, err := a.startDock(ctx, 300*time.Second); err != nil {
						a.notices.add("Assisted docking: "+err.Error(), noticeBad)
					}
				}(op.UpdatedAt)
			}
		}
	}
}

func runWindow(a *application, inputTest bool) error {
	w, h := WindowX, WindowY
	// Fit the default 1080p window on smaller laptop screens.
	// Monitor is nil while the display sleeps; keep the default size then.
	if mon := ebiten.Monitor(); mon != nil {
		if mw, mh := mon.Size(); mw > 0 && mh > 0 {
			scale := min(1, float64(mw)*0.85/float64(w), float64(mh)*0.85/float64(h))
			w, h = int(float64(w)*scale), int(float64(h)*scale)
		}
	}
	ebiten.SetWindowSize(w, h)
	ebiten.SetWindowTitle("ScoutDeck")
	if inputTest {
		ebiten.SetWindowTitle("ScoutDeck - Input Test (no robot)")
	}
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	// Keep Update running to immediately revoke manual input when focus is lost.
	ebiten.SetRunnableOnUnfocused(true)
	return ebiten.RunGame(newGame(a, inputTest))
}

func runInputTest(a *application) error {
	log.Print("Input test: no ROS connection; no commands will be sent to a robot")
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(control.Interval)
		defer t.Stop()
		var last control.Motion
		_ = a.control.Run(a.ctx, t.C, func(m control.Motion) error {
			if m != last {
				log.Printf("Input test motion: x=%.3f y=%.3f turn=%.3f", m.X, m.Y, m.Z)
				last = m
			}
			return nil
		})
	}()
	defer func() { a.cancel(); <-done }()
	return runWindow(a, true)
}

// sensorStatus adds the map origin to the sensor readings.
func (a *application) sensorStatus(now time.Time) map[string]any {
	out := a.sensors.status(now)
	out["map_origin"] = "start"
	if a.originAtDock() {
		out["map_origin"] = "dock"
	}
	out["odometry_feedback"] = a.sensors.tracker.OdometryValid()
	return out
}

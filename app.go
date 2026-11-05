package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msgs/geometry_msgs"
	"github.com/aler9/goroslib/pkg/msgs/std_msgs"
	"github.com/hajimehoshi/ebiten/v2"
	"github.com/shell-company/scout/internal/agent"
	"github.com/shell-company/scout/internal/control"
	"github.com/shell-company/scout/internal/docking"
)

var flagAPI = flag.String("api", "", "optional loopback HTTP address, e.g. 127.0.0.1:8080 (Bearer token: SCOUT_API_TOKEN)")
var flagHeadless = flag.Bool("headless", false, "run without a window; requires -api")
var flagSSH = flag.String("ssh", "", "optional SSH audio destination, e.g. root@robot.example (normal SSH authentication)")

type application struct {
	ctx          context.Context
	cancel       context.CancelFunc
	control      control.Controller
	actions      *control.Actions
	serviceMu    sync.Mutex
	pub          *goroslib.Publisher
	cancelDetect *goroslib.Publisher
	dock         *docking.Manager
	motionMu     sync.Mutex
}

func runApplication() (retErr error) {
	if *flagControlScheme != "keyboard" && *flagControlScheme != "joystick" {
		return fmt.Errorf("-c must be keyboard or joystick")
	}
	if *flagHeadless && *flagAPI == "" {
		return fmt.Errorf("-headless requires -api")
	}
	if *flagStream != "jpg" && *flagStream != "h264" {
		return fmt.Errorf("-stream must be jpg or h264")
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(ctx)
	a := &application{ctx: ctx, cancel: cancel, actions: control.NewActions()}
	var audio *agent.SSHAudio
	if *flagSSH != "" {
		var err error
		audio, err = agent.NewSSHAudio(*flagSSH)
		if err != nil {
			cancel()
			return err
		}
	}
	var subs []*goroslib.Subscriber
	var server *http.Server
	var done chan struct{}
	var controlErr error
	defer func() {
		cancel()
		if a.dock != nil && a.dock.Active() {
			cleanup, stop := context.WithTimeout(context.Background(), 9*time.Second)
			if err := a.dock.Cancel(cleanup); err != nil {
				log.Printf("docking cancellation failed: %v", err)
				if retErr == nil {
					retErr = err
				}
			}
			stop()
		}
		if done != nil {
			<-done
			if retErr == nil {
				retErr = controlErr
			}
		}
		if server != nil {
			_ = server.Close()
		}
		// Unblock any camera callback writing to ffmpeg before waiting on subscribers.
		if h264Dec != nil {
			h264Dec.Close()
		}
		for _, s := range subs {
			_ = s.Close()
		}
		if h264DumpFile != nil {
			_ = h264DumpFile.Close()
		}
		if a.pub != nil {
			_ = a.pub.Close()
		}
		if a.cancelDetect != nil {
			_ = a.cancelDetect.Close()
		}
		if n != nil {
			_ = n.Close()
		}
	}()
	if os.Getenv("EBITENGINE_GRAPHICS_LIBRARY") == "" {
		_ = os.Setenv("EBITENGINE_GRAPHICS_LIBRARY", "opengl")
	}
	if err := forceROSHostnameToMasterIP(*flagROSHostAddress, *flagVerbose); err != nil {
		return err
	}
	var err error
	n, err = goroslib.NewNode(goroslib.NodeConf{Name: fmt.Sprintf("scout_access_%d", os.Getpid()), MasterAddress: *flagROSHostAddress, Host: resolveNodeHost(*flagROSHostAddress, *flaglocalhost)})
	if err != nil {
		return err
	}
	a.pub, err = goroslib.NewPublisher(goroslib.PublisherConf{Node: n, Topic: "/cmd_vel", Msg: &geometry_msgs.Twist{}})
	if err != nil {
		return err
	}
	a.cancelDetect, err = goroslib.NewPublisher(goroslib.PublisherConf{Node: n, Topic: "/AppNode/cancel_detect", Msg: &std_msgs.Int32{}})
	if err != nil {
		return err
	}
	hooks := docking.Hooks{
		Prepare: func(ctx context.Context) error {
			if err := a.control.CheckAutonomy(time.Now()); err != nil {
				return err
			}
			a.control.Stop()
			a.motionMu.Lock()
			defer a.motionMu.Unlock()
			return a.publishRaw(control.Motion{})
		},
		Start:  scoutGoHome,
		Cancel: a.cancelDockNative,
	}
	if audio != nil {
		hooks.Probe = audio.Power
	}
	a.dock = docking.New(ctx, hooks)
	if *flagStream == "h264" {
		h264Dec, err = newH264Decoder()
		if err != nil {
			return err
		}
		if *flagDumpH264 != "" {
			h264DumpFile, err = os.Create(*flagDumpH264)
			if err != nil {
				return err
			}
		}
	}
	for _, conf := range []goroslib.SubscriberConf{
		{Node: n, Topic: "/CoreNode/going_home_status", Callback: func(s *std_msgs.Int32) { a.dock.Phase(s.Data) }, QueueSize: 8},
		{Node: n, Topic: streamTopicFromFlag(*flagStream), Callback: onMessageFrame},
		{Node: n, Topic: "/SensorNode/simple_battery_status", Callback: onBatteryStatus, QueueSize: 1},
		{Node: n, Topic: "/CoreNode/aac", Callback: func(f *Frame) {
			if f.Type == Frame_AUDIO_STREAM_AAC {
				media.Audio(f.Data)
			}
		}, QueueSize: 8},
	} {
		s, err := goroslib.NewSubscriber(conf)
		if err != nil {
			return err
		}
		subs = append(subs, s)
	}
	if *flagAPI != "" {
		host, _, err := net.SplitHostPort(*flagAPI)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("-api must bind a literal loopback IP; use an SSH tunnel for remote agents")
		}
		listener, err := net.Listen("tcp", *flagAPI)
		if err != nil {
			return err
		}
		token := os.Getenv("SCOUT_API_TOKEN")
		if token == "" {
			var b [32]byte
			if _, err := rand.Read(b[:]); err != nil {
				listener.Close()
				return err
			}
			token = hex.EncodeToString(b[:])
			log.Printf("Agent API Bearer token: %s", token)
		}
		api := &agent.API{Token: token, Control: &a.control, Media: media, Status: a.status, Action: a.action, Shutdown: cancel}
		api.StartDock = func(seconds int) (any, error) { return a.dock.Start(time.Duration(seconds) * time.Second) }
		api.DockStatus = func() any { return a.dock.Snapshot() }
		api.Stop = a.stopAll
		api.Drive = func(m control.Motion, ttl time.Duration) error {
			return a.dock.WhileIdle(func() error { return a.control.Drive(m, ttl, time.Now()) })
		}
		api.Record = (&agent.Microphone{Media: media}).Record
		if audio != nil {
			api.Play = audio.Play
			api.Power = func(ctx context.Context) (any, error) { return audio.Power(ctx) }
		}
		server = &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				log.Printf("agent API: %v", err)
				cancel()
			}
		}()
		log.Printf("Agent API listening at http://%s", listener.Addr())
	}
	go a.actions.Run(ctx, func(err error) {
		if err != nil {
			log.Printf("action failed: %v", err)
		}
	})
	done = make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(control.Interval)
		defer t.Stop()
		controlErr = a.control.Run(ctx, t.C, a.publish)
		cancel()
	}()
	log.Printf("Scout connected to ROS master %s; video=%s", *flagROSHostAddress, *flagStream)
	if *flagHeadless {
		<-ctx.Done()
		return nil
	}
	ebiten.SetWindowSize(WindowX, WindowY)
	ebiten.SetWindowTitle("Scout")
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	return ebiten.RunGame(&Game{app: a, speed: .2, keys: make(map[ebiten.Key]bool), wasFocused: true})
}

func (a *application) publish(m control.Motion) error {
	a.motionMu.Lock()
	defer a.motionMu.Unlock()
	if a.dock != nil && a.dock.Active() {
		if m != (control.Motion{}) {
			a.control.Stop()
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
				defer cancel()
				if err := a.dock.Cancel(ctx); err != nil {
					log.Printf("manual docking override failed: %v", err)
				}
			}()
		}
		return nil
	}
	return a.publishRaw(m)
}

func (a *application) publishRaw(m control.Motion) error {
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
		_, err := a.dock.Start(90 * time.Second)
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
	if a.dock != nil {
		dock = a.dock.Snapshot()
		if a.dock.Active() {
			ctrl.Owner = "docking"
		}
	}
	return map[string]any{"control": ctrl, "docking": dock, "battery": battery, "camera": map[string]any{"available": len(f.Data) > 0 && time.Since(f.At) < 3*time.Second, "observed_at": f.At}, "capabilities": map[string]any{"drive": true, "screenshot": true, "video": "mjpeg", "microphone": "aac", "ssh_audio": *flagSSH != "", "light": true, "dock": *flagSSH != ""}, "limits": map[string]any{"linear_x": .2, "linear_y": .3, "angular_z": 1.8, "max_ttl_ms": 500}}
}

package main

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msgs/nav_msgs"
	"github.com/aler9/goroslib/pkg/msgs/sensor_msgs"
	"github.com/shell-company/scout/internal/motion"
)

// sensors holds the latest readings from the robot's sensors and the
// dead-reckoning tracker fed by them.
type sensors struct {
	tracker motion.Tracker

	mu      sync.Mutex
	tof     motion.Range // filtered
	tofRaw  motion.RangeFilter
	night   int // -1 unknown, 0 day, 1 night (IR)
	limited time.Time
	objects []detection // recent detections from the robot, for the HUD
	// originAtDock: the tracker origin is the docked position.
	originAtDock bool
}

type detection struct {
	Name  string
	Score float64
	Box   [4]float64 // left, top, right, bottom (0..1)
	At    time.Time
}

func (s *sensors) setRange(r float64, now time.Time) {
	if math.IsNaN(r) || r < 0 {
		return
	}
	s.mu.Lock()
	s.tof = s.tofRaw.Add(motion.Range{Meters: r, At: now})
	s.mu.Unlock()
}

func (s *sensors) rangeReading() motion.Range {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tof
}

func (s *sensors) setLimited(now time.Time) {
	s.mu.Lock()
	s.limited = now
	s.mu.Unlock()
}

// limitedRecently reports whether the obstacle guard reduced a command in
// the last half second.
func (s *sensors) limitedRecently(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.limited) < 500*time.Millisecond
}

func (s *sensors) addDetection(d detection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.objects[:0]
	for _, o := range s.objects {
		if d.At.Sub(o.At) < 1500*time.Millisecond {
			keep = append(keep, o)
		}
	}
	s.objects = append(keep, d)
}

func (s *sensors) detections(now time.Time) []detection {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []detection
	for _, o := range s.objects {
		if now.Sub(o.At) < 1500*time.Millisecond {
			out = append(out, o)
		}
	}
	return out
}

// sensorStatus is the sensors section of /v1/status.
func (s *sensors) status(now time.Time) map[string]any {
	r := s.rangeReading()
	var dist any
	if r.Fresh(now) && !math.IsInf(r.Meters, 1) {
		dist = math.Round(r.Meters*1000) / 1000
	}
	s.mu.Lock()
	night := s.night
	s.mu.Unlock()
	pose := s.tracker.Pose()
	out := map[string]any{
		"obstacle_m":     dist, // null when nothing within 2 m or no fresh reading
		"range_fresh":    r.Fresh(now),
		"obstacle_guard": map[string]any{"stop_m": motion.GuardStop, "slow_m": motion.GuardSlow, "limiting": s.limitedRecently(now)},
		"heading_deg":    math.Round(pose.Yaw*180/math.Pi*10) / 10,
		"pose":           pose,
	}
	if night >= 0 {
		out["night_vision"] = night == 1
	}
	return out
}

// sensorSubscriptions are added to every robot connection.
func (a *application) sensorSubscriptions(node *goroslib.Node) []goroslib.SubscriberConf {
	s := &a.sensors
	return []goroslib.SubscriberConf{
		{Node: node, Topic: "/SensorNode/tof", QueueSize: 2, Callback: func(m *sensor_msgs.Range) {
			s.setRange(float64(m.Range), time.Now())
		}},
		{Node: node, Topic: "/SensorNode/imu", QueueSize: 16, Callback: func(m *sensor_msgs.Imu) {
			s.tracker.Gyro(m.AngularVelocity.Z, time.Now())
		}},
	}
}

// The wheel odometry topic is "relative": the firmware zeroes it when its
// first subscriber connects, and the firmware's own docking alignment relies
// on that. The app therefore lets go of it while the firmware docks (it
// crashed mid-alignment three times in testing with the app subscribed, and
// docked cleanly without it) and subscribes again afterwards.

func (a *application) subscribeOdometry() {
	a.odomMu.Lock()
	defer a.odomMu.Unlock()
	if !a.ready.Load() {
		return
	}
	node := rosNode()
	if a.odomSub != nil || node == nil {
		return
	}
	s, err := goroslib.NewSubscriber(goroslib.SubscriberConf{Node: node, Topic: "/MotorNode/baselink_odom_relative", QueueSize: 4,
		Callback: func(m *nav_msgs.Odometry) {
			a.sensors.tracker.Odometry(m.Twist.Twist.Linear.X, m.Twist.Twist.Linear.Y, time.Now())
		}})
	if err != nil {
		log.Printf("wheel odometry unavailable: %v", err)
		return
	}
	a.odomSub = s
}

func (a *application) unsubscribeOdometry() {
	a.odomMu.Lock()
	defer a.odomMu.Unlock()
	if a.odomSub != nil {
		_ = a.odomSub.Close()
		a.odomSub = nil
	}
}

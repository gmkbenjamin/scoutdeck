//go:build live

package main

// Read-only checks against a real robot: go test -tags live -run Live -v .
// with SCOUTDECK_LIVE_HOST=<robot ip> (or SCOUT_LIVE_HOST). Nothing here moves the robot.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msgs/nav_msgs"
	"github.com/aler9/goroslib/pkg/msgs/sensor_msgs"
)

func liveNode(t *testing.T) {
	host := os.Getenv("SCOUTDECK_LIVE_HOST")
	if host == "" {
		host = os.Getenv("SCOUT_LIVE_HOST")
	}
	if host == "" {
		t.Skip("SCOUTDECK_LIVE_HOST not set")
	}
	master := host + ":11311"
	if err := forceROSHostnameToMasterIP(master, false); err != nil {
		t.Fatal(err)
	}
	var err error
	n, err = goroslib.NewNode(goroslib.NodeConf{Name: fmt.Sprintf("scoutdeck_live_test_%d", os.Getpid()), MasterAddress: master, Host: resolveNodeHost(master, "")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close(); n = nil })
}

func TestLiveServices(t *testing.T) {
	liveNode(t)
	ctx := context.Background()
	var files recordGetFilesRes
	if err := callQuick(ctx, "/RecorderAgentNode/record_get_files", &recordGetFilesSrv{}, &recordGetFilesReq{Type: 0, Start: 0, Size: 20}, &files); err != nil {
		t.Fatal(err)
	}
	t.Logf("recordings: %+v", files.Files)
	if len(files.Files) > 0 {
		var p recordPathRes
		if err := callQuick(ctx, "/RecorderAgentNode/record_get_file_path", &recordGetPathSrv{}, &recordIDReq{Id: files.Files[0].Id}, &p); err != nil {
			t.Fatal(err)
		}
		t.Logf("path: %s", p.Path)
	}
	var cfg reconfigureRes
	if err := callQuick(ctx, "/ParamNode/video/set_parameters", &reconfigureSrv{}, &reconfigureReq{}, &cfg); err != nil {
		t.Fatal(err)
	}
	t.Logf("video config: %+v", cfg.Config.Ints)
	var night nightGetRes
	if err := callQuick(ctx, "/CoreNode/night_get", &nightGetSrv{}, &nightGetReq{}, &night); err != nil {
		t.Fatal(err)
	}
	t.Logf("night: %+v", night)
}

func TestLiveSensors(t *testing.T) {
	liveNode(t)
	got := map[string]int{}
	done := make(chan struct{})
	var subs []*goroslib.Subscriber
	add := func(topic string, cb any) {
		s, err := goroslib.NewSubscriber(goroslib.SubscriberConf{Node: n, Topic: topic, Callback: cb, QueueSize: 4})
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, s)
	}
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	count := func(k string) { <-mu; got[k]++; mu <- struct{}{} }
	var lastRange float32
	add("/SensorNode/tof", func(m *sensor_msgs.Range) { lastRange = m.Range; count("tof") })
	add("/SensorNode/imu", func(m *sensor_msgs.Imu) { count("imu") })
	add("/SensorNode/light", func(m *sensor_msgs.Illuminance) { count("light") })
	add("/MotorNode/baselink_odom_relative", func(m *nav_msgs.Odometry) { count("odom") })
	add("/CoreNode/obj", func(m *detectMsg) { t.Logf("obj: %+v", *m); count("obj") })
	time.AfterFunc(3*time.Second, func() { close(done) })
	<-done
	for _, s := range subs {
		s.Close()
	}
	<-mu
	t.Logf("messages in 3 s: %v, last range %v", got, lastRange)
	if got["tof"] == 0 || got["imu"] == 0 || got["odom"] == 0 {
		t.Fatalf("missing sensor data: %v", got)
	}
}

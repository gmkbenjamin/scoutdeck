package main

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msg"
)

// roller_eye/wifi_info
type wifiInfo struct {
	msg.Package `ros:"roller_eye"`
	SSID        string  `rosname:"ssid"`
	Quality     float32 `rosname:"quality"`
	Signal      int32   `rosname:"signal"`
	Noisy       int32   `rosname:"noisy"`
	Freq        float32 `rosname:"freq"`
	Channel     int32   `rosname:"channel"`
}

type wifiScanRequest struct{}
type wifiScanResponse struct {
	Result []wifiInfo `rosname:"result"`
}

// roller_eye/wifi_scan_list
type wifiScanService struct {
	msg.Package `ros:"roller_eye"`
	Request     wifiScanRequest
	Response    wifiScanResponse
}

type wifiSwitchRequest struct {
	Mode int8   `rosname:"mode"`
	SSID string `rosname:"ssid"`
	Key  string `rosname:"key"`
}
type wifiSwitchResponse struct {
	Status int8 `rosname:"status"`
}

// roller_eye/wifi_switch_ssid_key
type wifiSwitchService struct {
	msg.Package `ros:"roller_eye"`
	Request     wifiSwitchRequest
	Response    wifiSwitchResponse
}

// wifiModeStation is the firmware's mode for joining a network. WiFiNode logs
// "switchMode 0" when it connects to saved credentials and 1 for its hotspot;
// this is the reverse of the WIFI_MODE_* constants in roller_eye/status.
const wifiModeStation = 0

func callService(ctx context.Context, name string, srv, req, res any) error {
	node := rosNode()
	if node == nil {
		return fmt.Errorf("not connected to the robot")
	}
	cl, err := goroslib.NewServiceClient(goroslib.ServiceClientConf{Node: node, Name: name, Srv: srv})
	if err != nil {
		return err
	}
	defer cl.Close()
	return cl.CallContext(ctx, req, res)
}

// scanWiFi asks the robot which networks it can see, strongest first.
func scanWiFi(parent context.Context) ([]wifiInfo, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	var res wifiScanResponse
	if err := callService(ctx, "/WiFiNode/wifi_scan_list", &wifiScanService{}, &wifiScanRequest{}, &res); err != nil {
		return nil, fmt.Errorf("Wi-Fi scan failed: %w", err)
	}
	best := map[string]wifiInfo{}
	for _, w := range res.Result {
		if w.SSID == "" || strings.ContainsAny(w.SSID, "\n\r\x00") {
			continue
		}
		if old, ok := best[w.SSID]; !ok || w.Signal > old.Signal {
			best[w.SSID] = w
		}
	}
	out := make([]wifiInfo, 0, len(best))
	for _, w := range best {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Signal > out[j].Signal })
	return out, nil
}

// switchWiFi asks the firmware to join a network now. The robot drops off the
// current network while it switches, so a missing reply is expected.
func switchWiFi(parent context.Context, ssid, key string) error {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	var res wifiSwitchResponse
	err := callService(ctx, "/WiFiNode/switch_wifi_ssidkey", &wifiSwitchService{}, &wifiSwitchRequest{Mode: wifiModeStation, SSID: ssid, Key: key}, &res)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("robot rejected the Wi-Fi change: %w", err)
	}
	return nil
}

// hotspotAddress is the robot's own address in hotspot mode.
const hotspotAddress = "10.42.0.1"

// discoverRobots looks for ROS masters (TCP 11311) on the Mac's local IPv4
// networks, scanning at most the /24 around each address, plus the hotspot.
func discoverRobots(ctx context.Context) []string {
	candidates := map[string]bool{hotspotAddress: true}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			for i := 1; i < 255; i++ {
				c := net.IPv4(ip[0], ip[1], ip[2], byte(i)).String()
				if c != ip.String() {
					candidates[c] = true
				}
			}
		}
	}
	var (
		mu    sync.Mutex
		found []string
		wg    sync.WaitGroup
		slots = make(chan struct{}, 64)
	)
	for c := range candidates {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			d := net.Dialer{Timeout: 600 * time.Millisecond}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "11311"))
			if err != nil {
				return
			}
			conn.Close()
			mu.Lock()
			found = append(found, host)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool {
		a, b := net.ParseIP(found[i]).To4(), net.ParseIP(found[j]).To4()
		return string(a) < string(b)
	})
	return found
}

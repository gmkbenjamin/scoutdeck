package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gmkbenjamin/scoutdeck/internal/agent"
	"github.com/gmkbenjamin/scoutdeck/internal/motion"
)

// ---------- Camera settings (/ParamNode/video, dynamic_reconfigure) ----------

// CameraSetting describes one adjustable video parameter.
type CameraSetting struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Min     int32    `json:"min"`
	Max     int32    `json:"max"`
	Value   int32    `json:"value"`
	Choices []string `json:"choices,omitempty"` // labels for Min..Max, when they are named
}

// cameraParams are the firmware video parameters exposed to users. Resolution
// and frame rate are left alone: the firmware's streams depend on them.
var cameraParams = []CameraSetting{
	{Name: "image_night_mode", Label: "Night vision (IR)", Min: 0, Max: 2, Choices: []string{"Off", "On", "Auto"}},
	{Name: "nightVisionSensitivity", Label: "Night vision sensitivity", Min: 0, Max: 1, Choices: []string{"Normal", "High"}},
	{Name: "cameraLight", Label: "Brightness", Min: 0, Max: 100},
	{Name: "wideDynamic", Label: "Wide dynamic range", Min: 0, Max: 1, Choices: []string{"Auto", "Manual"}},
	{Name: "wideDynamicRange", Label: "WDR level (manual)", Min: 0, Max: 5},
	{Name: "h264Quality", Label: "Stream compression", Min: 0, Max: 2, Choices: []string{"Best quality", "Balanced", "Smallest"}},
}

func cameraParam(name string) (CameraSetting, bool) {
	for _, p := range cameraParams {
		if p.Name == name {
			return p, true
		}
	}
	return CameraSetting{}, false
}

// CameraSettings reads the current values (an empty reconfigure request
// returns the full configuration without changing anything).
func (a *application) CameraSettings(ctx context.Context) ([]CameraSetting, error) {
	return a.reconfigure(ctx, nil)
}

// SetCamera changes one camera parameter and returns all of them.
func (a *application) SetCamera(ctx context.Context, name string, value int32) ([]CameraSetting, error) {
	p, ok := cameraParam(name)
	if !ok {
		return nil, fmt.Errorf("unknown camera setting %q", name)
	}
	if value < p.Min || value > p.Max {
		return nil, fmt.Errorf("%s must be %d..%d", p.Label, p.Min, p.Max)
	}
	out, err := a.reconfigure(ctx, []drInt{{Name: name, Value: value}})
	if err == nil {
		log.Printf("camera: %s = %d", name, value)
	}
	return out, err
}

func (a *application) reconfigure(ctx context.Context, ints []drInt) ([]CameraSetting, error) {
	if !a.ready.Load() {
		return nil, fmt.Errorf("robot not connected")
	}
	if a.dockingActive() {
		return nil, fmt.Errorf("camera settings are unavailable while docking")
	}
	var res reconfigureRes
	if err := callQuick(ctx, "/ParamNode/video/set_parameters", &reconfigureSrv{}, &reconfigureReq{Config: drConfig{Ints: ints}}, &res); err != nil {
		return nil, err
	}
	values := map[string]int32{}
	for _, v := range res.Config.Ints {
		values[v.Name] = v.Value
	}
	out := make([]CameraSetting, 0, len(cameraParams))
	for _, p := range cameraParams {
		if v, ok := values[p.Name]; ok {
			p.Value = v
			out = append(out, p)
		}
	}
	a.cameraCache.Lock()
	a.cameraCache.list, a.cameraCache.at = out, time.Now()
	a.cameraCache.Unlock()
	return out, nil
}

type cameraCache struct {
	sync.Mutex
	list []CameraSetting
	at   time.Time
}

// ---------- Recordings stored on the robot ----------

// RobotRecording is one file the firmware recorded (its own motion alerts,
// schedules or snapshots).
type RobotRecording struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"` // video, snapshot, scheduled
	Seconds  uint32    `json:"seconds"`
	Bytes    uint32    `json:"bytes"`
	Created  time.Time `json:"created"`
	MimeType string    `json:"mime_type"`
}

var recordingID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// RobotRecordings lists files on the robot, newest first.
func (a *application) RobotRecordings(ctx context.Context) ([]RobotRecording, error) {
	if !a.ready.Load() {
		return nil, fmt.Errorf("robot not connected")
	}
	var res recordGetFilesRes
	if err := callQuick(ctx, "/RecorderAgentNode/record_get_files", &recordGetFilesSrv{}, &recordGetFilesReq{Type: 0, Start: 0, Size: 200}, &res); err != nil {
		return nil, err
	}
	out := []RobotRecording{}
	for _, f := range res.Files {
		if !recordingID.MatchString(f.Id) {
			continue
		}
		kind, mime := "video", "video/mp4"
		switch f.Type {
		case 1:
			kind, mime = "snapshot", "image/jpeg"
		case 3:
			continue // thumbnails belong to videos
		case 4:
			kind = "scheduled"
		}
		if strings.HasSuffix(strings.ToLower(f.Name), ".jpg") {
			kind, mime = "snapshot", "image/jpeg"
		}
		out = append(out, RobotRecording{ID: f.Id, Name: filepath.Base(f.Name), Kind: kind, Seconds: f.Dur, Bytes: f.Size, Created: f.Create, MimeType: mime})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// robotRecordingPath asks the firmware where a recording lives.
func (a *application) robotRecordingPath(ctx context.Context, id string) (string, error) {
	if !recordingID.MatchString(id) {
		return "", fmt.Errorf("invalid recording id")
	}
	var res recordPathRes
	if err := callQuick(ctx, "/RecorderAgentNode/record_get_file_path", &recordGetPathSrv{}, &recordIDReq{Id: id}, &res); err != nil {
		return "", err
	}
	if !agent.ValidMediaPath(res.Path) {
		return "", fmt.Errorf("recording not found")
	}
	return res.Path, nil
}

// DeleteRobotRecording removes a recording from the robot.
func (a *application) DeleteRobotRecording(ctx context.Context, id string) error {
	if !recordingID.MatchString(id) {
		return fmt.Errorf("invalid recording id")
	}
	var res recordDeleteRes
	if err := callQuick(ctx, "/RecorderAgentNode/record_delete_file", &recordDeleteSrv{}, &recordIDReq{Id: id}, &res); err != nil {
		return err
	}
	if res.Status != 0 {
		return fmt.Errorf("the robot could not delete it (status %d)", res.Status)
	}
	return nil
}

// DownloadRobotRecording copies a recording to dir on the Mac and returns its path.
func (a *application) DownloadRobotRecording(ctx context.Context, id, dir string) (string, error) {
	if a.audio == nil {
		return "", fmt.Errorf("downloading recordings needs SSH")
	}
	path, err := a.robotRecordingPath(ctx, id)
	if err != nil {
		return "", err
	}
	base := "robot-" + filepath.Base(path)
	f, err := os.CreateTemp(dir, ".scoutdeck-download-*")
	if err != nil {
		return "", err
	}
	w := bufio.NewWriter(f)
	err = a.audio.ReadMedia(ctx, path, w)
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	var dst string
	if err == nil {
		dst, err = linkUniqueFile(f.Name(), dir, base)
	}
	_ = os.Remove(f.Name())
	if err != nil {
		return "", err
	}
	return dst, nil
}

// linkUniqueFile moves src into dir using base or base-N before its extension,
// without overwriting any existing download.
func linkUniqueFile(src, dir, base string) (string, error) {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		dst := filepath.Join(dir, name)
		err := os.Link(src, dst)
		if err == nil {
			return dst, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		// Fallback for filesystems without hardlinks, only if unused.
		if _, statErr := os.Lstat(dst); errors.Is(statErr, os.ErrNotExist) {
			return dst, os.Rename(src, dst)
		}
	}
	return "", fmt.Errorf("too many copies of %s", base)
}

// ---------- Night mode and battery history ----------

// batterySample is one point in the battery history.
type batterySample struct {
	At       time.Time `json:"t"`
	Percent  int       `json:"p"`
	Charging bool      `json:"c"`
}

type batteryHistory struct {
	mu      sync.Mutex
	samples []batterySample
	loaded  bool
}

const batteryHistoryMax = 7 * 24 * 60 // one week of minutes

func batteryHistoryPath() (string, error) {
	dir, err := dataDir("")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "battery.jsonl"), nil
}

func (b *batteryHistory) load() {
	if b.loaded {
		return
	}
	b.loaded = true
	path, err := batteryHistoryPath()
	if err != nil {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s batterySample
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.Percent >= 0 && s.Percent <= 100 {
			b.samples = append(b.samples, s)
		}
	}
	if len(b.samples) > batteryHistoryMax {
		b.samples = b.samples[len(b.samples)-batteryHistoryMax:]
		b.rewrite()
	}
}

func (b *batteryHistory) rewrite() {
	path, err := batteryHistoryPath()
	if err != nil {
		return
	}
	var sb strings.Builder
	for _, s := range b.samples {
		data, _ := json.Marshal(s)
		sb.Write(data)
		sb.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err == nil {
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
		}
	}
}

func (b *batteryHistory) add(s batterySample) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	b.samples = append(b.samples, s)
	if len(b.samples) > batteryHistoryMax+600 {
		b.samples = b.samples[len(b.samples)-batteryHistoryMax:]
		b.rewrite()
		return
	}
	path, err := batteryHistoryPath()
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	data, _ := json.Marshal(s)
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}

// since returns samples newer than t.
func (b *batteryHistory) since(t time.Time) []batterySample {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.load()
	var out []batterySample
	for _, s := range b.samples {
		if s.At.After(t) {
			out = append(out, s)
		}
	}
	return out
}

// watchRobot polls slow-changing robot state: night mode every 15 s and a
// battery sample every minute.
func (a *application) watchRobot(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	var lastSample, lastNight time.Time
	for {
		// night_get is served by the firmware's camera node, which also runs
		// docking. Ask only while it is idle, and rarely: the node crashed
		// twice in testing while aligning to the dock with this call in flight.
		docking := a.dockingActive()
		if !docking && a.act.snapshot(motion.Pose{}).Kind == "" && time.Since(lastNight) >= time.Minute {
			lastNight = time.Now()
			var night nightGetRes
			call, cancel := context.WithTimeout(ctx, 4*time.Second)
			if err := callQuick(call, "/CoreNode/night_get", &nightGetSrv{}, &nightGetReq{}, &night); err == nil {
				a.sensors.mu.Lock()
				a.sensors.night = int(night.IsNight)
				a.sensors.mu.Unlock()
			}
			cancel()
		}
		batteryMu.RLock()
		pct, charge, at := batteryPercent, batteryChargeState, batteryAt
		batteryMu.RUnlock()
		if pct >= 0 && time.Since(at) < batteryFreshFor && time.Since(lastSample) >= 59*time.Second {
			lastSample = time.Now()
			a.battery.add(batterySample{At: lastSample.Truncate(time.Second), Percent: pct, Charging: charge == 0 || charge == 2})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Health combines the robot's vital signs with the Mac's view of the connection.
func (a *application) Health(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	if a.audio != nil {
		h, err := a.audio.Health(ctx)
		if err != nil {
			out["error"] = err.Error()
		}
		for k, v := range h {
			out[k] = v
		}
	} else {
		out["error"] = "detailed health needs SSH"
	}
	out["battery_history"] = a.battery.since(time.Now().Add(-24 * time.Hour))
	out["odometry_feedback"] = a.sensors.tracker.OdometryValid()
	out["events_today"] = a.events.Counts(time.Now().Truncate(24 * time.Hour))
	return out, nil
}

// watchPower reads the battery over SSH every few seconds. The firmware's
// battery topic is latched and republished rarely, so without this the
// charging state lags by minutes after docking or undocking.
func (a *application) watchPower(ctx context.Context) {
	if a.audio == nil {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		status, pct, err := a.audio.Battery(ctx)
		if err == nil {
			state := 3
			switch status {
			case "Charging":
				state = 0
			case "Discharging":
				state = 1
			case "Full", "Not charging":
				state = 2
			}
			batteryMu.Lock()
			batteryPercent, batteryChargeState, batteryAt = pct, state, time.Now()
			batteryMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shell-company/scout/internal/agent"
	"github.com/shell-company/scout/internal/motion"
)

// offlineOK lists API paths that work without a robot connection.
func offlineOK(path string) bool {
	switch path {
	case "/v1/status", "/v1/shutdown", "/v1/autodock", "/v1/guard", "/v1/obstacle-guard", "/v1/routes":
		return true
	}
	return strings.HasPrefix(path, "/v1/events")
}

// registerFeatures adds moves, sensors, guard mode, events, recording,
// routes, camera settings and health to the agent API.
func (a *application) registerFeatures(mux *http.ServeMux) {
	writeJSON, readJSON, fail := agent.WriteJSON, agent.ReadJSON, agent.Fail

	runMove := func(w http.ResponseWriter, r *http.Request, g motion.Goal) {
		res, err := a.Move(r.Context(), g)
		if err != nil {
			fail(w, 409, err)
			return
		}
		writeJSON(w, 200, res)
	}
	mux.HandleFunc("POST /v1/move", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Distance *float64 `json:"distance_m"`
			Strafe   *float64 `json:"strafe_m"`
			Speed    float64  `json:"speed"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		switch {
		case in.Distance != nil && in.Strafe == nil:
			runMove(w, r, motion.Goal{Kind: motion.Drive, Target: *in.Distance, Speed: in.Speed})
		case in.Strafe != nil && in.Distance == nil:
			runMove(w, r, motion.Goal{Kind: motion.Strafe, Target: *in.Strafe, Speed: in.Speed})
		default:
			fail(w, 400, fmt.Errorf("give exactly one of distance_m (forward +, back -) or strafe_m (left +, right -)"))
		}
	})
	mux.HandleFunc("POST /v1/turn", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Degrees float64 `json:"degrees"`
			Rate    float64 `json:"rate"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		runMove(w, r, motion.Goal{Kind: motion.Turn, Target: in.Degrees * math.Pi / 180, Speed: in.Rate})
	})
	mux.HandleFunc("GET /v1/pose", func(w http.ResponseWriter, r *http.Request) {
		trail := a.sensors.tracker.Trail()
		if step := len(trail)/500 + 1; step > 1 {
			thin := make([]motion.Point, 0, 501)
			for i := 0; i < len(trail); i += step {
				thin = append(thin, trail[i])
			}
			trail = append(thin, trail[len(trail)-1])
		}
		writeJSON(w, 200, map[string]any{"pose": a.sensors.tracker.Pose(), "trail": trail, "odometry_feedback": a.sensors.tracker.OdometryValid()})
	})
	mux.HandleFunc("POST /v1/pose/reset", func(w http.ResponseWriter, r *http.Request) {
		a.sensors.tracker.Reset(time.Now())
		writeJSON(w, 200, a.sensors.tracker.Pose())
	})

	// Obstacle guard for manual driving.
	mux.HandleFunc("GET /v1/obstacle-guard", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"manual": a.manualObstacleGuard(), "stop_m": motion.GuardStop, "slow_m": motion.GuardSlow})
	})
	mux.HandleFunc("POST /v1/obstacle-guard", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Manual *bool `json:"manual"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.Manual == nil {
			fail(w, 400, fmt.Errorf("manual is required"))
			return
		}
		if err := a.setManualObstacleGuard(*in.Manual); err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"manual": *in.Manual})
	})

	// Guard mode and the event timeline.
	mux.HandleFunc("GET /v1/guard", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.guardSettings()) })
	mux.HandleFunc("POST /v1/guard", func(w http.ResponseWriter, r *http.Request) {
		in := a.guardSettings()
		if !readJSON(w, r, &in) {
			return
		}
		if err := a.setGuard(in); err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, 200, a.guardSettings())
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 300 {
				fail(w, 400, fmt.Errorf("limit must be 1..300"))
				return
			}
			limit = n
		}
		var since time.Time
		if v := r.URL.Query().Get("since"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				fail(w, 400, fmt.Errorf("since must be an RFC 3339 time"))
				return
			}
			since = t
		}
		writeJSON(w, 200, map[string]any{"events": a.events.List(limit, since)})
	})
	mux.HandleFunc("GET /v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(r.PathValue("id"), ".jpg")
		data, err := a.events.Snapshot(id)
		if err != nil {
			fail(w, 404, fmt.Errorf("no snapshot for that event"))
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("DELETE /v1/events", func(w http.ResponseWriter, r *http.Request) {
		if err := a.events.Clear(); err != nil {
			fail(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"cleared": true})
	})

	// Local recording on the Mac.
	mux.HandleFunc("POST /v1/record/start", func(w http.ResponseWriter, r *http.Request) {
		path, err := a.rec.Start(a.ctx, mediaDir())
		if err != nil {
			fail(w, 409, err)
			return
		}
		a.notices.add("Recording to "+filepath.Base(path), noticeInfo)
		writeJSON(w, 200, a.rec.status())
	})
	mux.HandleFunc("POST /v1/record/stop", func(w http.ResponseWriter, r *http.Request) {
		path, err := a.rec.Stop()
		if err != nil {
			fail(w, 409, err)
			return
		}
		a.notices.add("Recording saved: "+path, noticeGood)
		writeJSON(w, 200, map[string]string{"file": path})
	})

	// Recordings stored on the robot.
	mux.HandleFunc("GET /v1/robot-recordings", func(w http.ResponseWriter, r *http.Request) {
		list, err := a.RobotRecordings(r.Context())
		if err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, 200, map[string]any{"recordings": list})
	})
	mux.HandleFunc("GET /v1/robot-recordings/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.audio == nil {
			fail(w, 503, fmt.Errorf("downloading recordings needs SSH"))
			return
		}
		path, err := a.robotRecordingPath(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, 404, err)
			return
		}
		kind := "video/mp4"
		if strings.HasSuffix(strings.ToLower(path), ".jpg") {
			kind = "image/jpeg"
		}
		// Buffer so a failed read becomes an error response, not a truncated file.
		var buf bytes.Buffer
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		if err := a.audio.ReadMedia(ctx, path, &limitedBuffer{&buf, 200 << 20}); err != nil {
			fail(w, 502, err)
			return
		}
		w.Header().Set("Content-Type", kind)
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filepath.Base(path)))
		http.ServeContent(w, r, filepath.Base(path), time.Time{}, bytes.NewReader(buf.Bytes()))
	})
	mux.HandleFunc("DELETE /v1/robot-recordings/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.DeleteRobotRecording(r.Context(), r.PathValue("id")); err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
	})

	// Camera settings.
	mux.HandleFunc("GET /v1/camera", func(w http.ResponseWriter, r *http.Request) {
		list, err := a.CameraSettings(r.Context())
		if err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, 200, map[string]any{"settings": list})
	})
	mux.HandleFunc("POST /v1/camera", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name  string `json:"name"`
			Value *int32 `json:"value"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.Value == nil {
			fail(w, 400, fmt.Errorf("value is required"))
			return
		}
		list, err := a.SetCamera(r.Context(), in.Name, *in.Value)
		if err != nil {
			fail(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"settings": list})
	})

	// Patrol routes.
	mux.HandleFunc("GET /v1/routes", func(w http.ResponseWriter, r *http.Request) {
		name, steps, dur := a.routes.recordingRoute()
		rec := map[string]any{"active": name != ""}
		if name != "" {
			rec["name"], rec["steps"], rec["seconds"] = name, steps, dur.Seconds()
		}
		writeJSON(w, 200, map[string]any{"routes": a.routes.List(), "recording": rec})
	})
	mux.HandleFunc("POST /v1/routes/record", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if err := a.StartRecording(strings.TrimSpace(in.Name)); err != nil {
			fail(w, 409, err)
			return
		}
		a.notices.add("Recording route "+in.Name+": drive it, then save", noticeInfo)
		writeJSON(w, 200, map[string]any{"recording": in.Name})
	})
	mux.HandleFunc("POST /v1/routes/record/stop", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Discard bool `json:"discard"`
		}
		if r.ContentLength != 0 && !readJSON(w, r, &in) {
			return
		}
		route, err := a.FinishRecording(in.Discard)
		if err != nil {
			fail(w, 409, err)
			return
		}
		if in.Discard {
			writeJSON(w, 200, map[string]bool{"discarded": true})
			return
		}
		a.notices.add(fmt.Sprintf("Route saved: %s (%d steps, %.1f m)", route.Name, len(route.Steps), route.Distance), noticeGood)
		writeJSON(w, 200, route)
	})
	mux.HandleFunc("POST /v1/routes/{name}/run", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Patrol(r.Context(), r.PathValue("name")); err != nil {
			fail(w, 409, err)
			return
		}
		writeJSON(w, 202, a.act.snapshot(a.sensors.tracker.Pose()))
	})
	mux.HandleFunc("POST /v1/routes/{name}", func(w http.ResponseWriter, r *http.Request) {
		cur, ok := a.routes.Get(r.PathValue("name"))
		if !ok {
			fail(w, 404, fmt.Errorf("no such route"))
			return
		}
		in := struct {
			Every     int  `json:"every_minutes"`
			DockAfter bool `json:"dock_after"`
		}{cur.EveryMinutes, cur.DockAfter}
		if !readJSON(w, r, &in) {
			return
		}
		route, err := a.routes.Update(cur.Name, in.Every, in.DockAfter)
		if err != nil {
			fail(w, 400, err)
			return
		}
		writeJSON(w, 200, route)
	})
	mux.HandleFunc("DELETE /v1/routes/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.routes.Delete(r.PathValue("name")); err != nil {
			fail(w, 404, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
	})

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		h, err := a.Health(r.Context())
		if err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, 200, h)
	})
	mux.HandleFunc("POST /v1/say", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Text string `json:"text"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if err := a.Say(r.Context(), in.Text); err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"spoken": true})
	})
}

// limitedBuffer refuses writes past a size limit.
type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.b.Len()+len(p) > l.max {
		return 0, fmt.Errorf("file is too large")
	}
	return l.b.Write(p)
}

func (a *application) manualObstacleGuard() bool {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	return !a.settings.ObstacleGuardOff
}

func (a *application) setManualObstacleGuard(on bool) error {
	a.settingsMu.Lock()
	a.settings.ObstacleGuardOff = !on
	a.settingsMu.Unlock()
	return a.saveSettings()
}

// Say speaks text through the robot's speaker using the Mac's voice.
func (a *application) Say(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 500 {
		return fmt.Errorf("text must be 1-500 characters")
	}
	if a.audio == nil {
		return fmt.Errorf("the robot's speaker needs SSH")
	}
	tmp, err := os.MkdirTemp("", "scout-say-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "say.wav")
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Text goes on stdin (-f -), never on the command line.
	cmd := exec.CommandContext(call, "say", "-o", out, "--file-format=WAVE", "--data-format=LEI16@16000", "-f", "-")
	cmd.Stdin = strings.NewReader(text)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("text to speech failed: %v %s", err, msg)
	}
	wav, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	return a.audio.Play(ctx, wav)
}

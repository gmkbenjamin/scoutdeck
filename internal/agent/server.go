// Package agent exposes a bounded, authenticated HTTP interface for local agents.
package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shell-company/scout/internal/control"
)

type API struct {
	Token      string
	Control    *control.Controller
	Media      *control.Media
	Status     func() any
	Action     func(context.Context, string, int) error
	Record     func(context.Context, int) ([]byte, error)
	Play       func(context.Context, []byte) error
	Shutdown   func()
	Drive      func(control.Motion, time.Duration) error
	Stop       func(context.Context) error
	StartDock  func(int) (any, error)
	DockStatus func() any
	Power      func(context.Context) (any, error)
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.Status()) })
	mux.HandleFunc("POST /v1/drive", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			control.Motion
			TTL int `json:"ttl_ms"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.TTL == 0 {
			in.TTL = 300
		}
		if in.TTL < 100 || in.TTL > 500 {
			fail(w, 409, fmt.Errorf("ttl_ms must be 100..500"))
			return
		}
		drive := a.Drive
		if drive == nil {
			drive = func(m control.Motion, ttl time.Duration) error { return a.Control.Drive(m, ttl, time.Now()) }
		}
		if err := drive(in.Motion, time.Duration(in.TTL)*time.Millisecond); err != nil {
			fail(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]any{"accepted": true, "ttl_ms": in.TTL})
	})
	mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) {
		a.Control.Stop()
		if a.Stop != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			if err := a.Stop(ctx); err != nil {
				fail(w, 502, err)
				return
			}
		}
		writeJSON(w, 200, map[string]bool{"accepted": true})
	})
	startDock := func(w http.ResponseWriter, r *http.Request) {
		if a.StartDock == nil {
			fail(w, 503, fmt.Errorf("docking is not configured"))
			return
		}
		in := struct {
			Timeout int `json:"timeout_seconds"`
		}{Timeout: 90}
		if r.ContentLength != 0 && !readJSON(w, r, &in) {
			return
		}
		if in.Timeout < 10 || in.Timeout > 180 {
			fail(w, 400, fmt.Errorf("timeout_seconds must be 10..180"))
			return
		}
		op, err := a.StartDock(in.Timeout)
		if err != nil {
			fail(w, 409, err)
			return
		}
		writeJSON(w, 202, op)
	}
	mux.HandleFunc("POST /v1/dock", startDock)
	mux.HandleFunc("POST /v1/home", func(w http.ResponseWriter, r *http.Request) {
		if a.StartDock != nil {
			startDock(w, r)
			return
		}
		a.Control.Stop()
		a.action(w, r, "home", 0)
	})
	mux.HandleFunc("GET /v1/dock", func(w http.ResponseWriter, r *http.Request) {
		if a.DockStatus == nil {
			fail(w, 503, fmt.Errorf("docking is not configured"))
			return
		}
		writeJSON(w, 200, a.DockStatus())
	})
	mux.HandleFunc("POST /v1/dock/cancel", func(w http.ResponseWriter, r *http.Request) {
		if a.Stop == nil || a.DockStatus == nil {
			fail(w, 503, fmt.Errorf("docking is not configured"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := a.Stop(ctx); err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, 200, a.DockStatus())
	})
	mux.HandleFunc("GET /v1/power", func(w http.ResponseWriter, r *http.Request) {
		if a.Power == nil {
			fail(w, 503, fmt.Errorf("live charging checks require -ssh"))
			return
		}
		p, err := a.Power(r.Context())
		if err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("POST /v1/light", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Command *int `json:"command"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.Command == nil || (*in.Command != 0 && *in.Command != 1 && *in.Command != 3 && *in.Command != 4) {
			fail(w, 400, fmt.Errorf("command must be 0 (down), 1 (up), 3 (max), or 4 (auto)"))
			return
		}
		a.action(w, r, "light", *in.Command)
	})
	mux.HandleFunc("GET /v1/screenshot.jpg", func(w http.ResponseWriter, r *http.Request) {
		f := a.Media.Frame()
		if len(f.Data) == 0 || time.Since(f.At) > 3*time.Second {
			fail(w, 503, fmt.Errorf("no fresh camera frame"))
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Frame-Time", f.At.UTC().Format(time.RFC3339Nano))
		w.Write(f.Data)
	})
	mux.HandleFunc("GET /v1/video.mjpeg", a.video)
	mux.HandleFunc("GET /v1/microphone.aac", a.microphone)
	mux.HandleFunc("GET /v1/microphone.wav", func(w http.ResponseWriter, r *http.Request) {
		seconds := 3
		if v := r.URL.Query().Get("seconds"); v != "" {
			var err error
			seconds, err = strconv.Atoi(v)
			if err != nil {
				fail(w, 400, err)
				return
			}
		}
		if seconds < 1 || seconds > 10 {
			fail(w, 400, fmt.Errorf("seconds must be 1..10"))
			return
		}
		if a.Record == nil {
			fail(w, 503, fmt.Errorf("microphone recording is not configured"))
			return
		}
		data, err := a.Record(r.Context(), seconds)
		if err != nil {
			fail(w, 502, err)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(data)
	})
	mux.HandleFunc("POST /v1/speaker.wav", func(w http.ResponseWriter, r *http.Request) {
		if a.Play == nil {
			fail(w, 503, fmt.Errorf("SSH audio is not configured"))
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1000000))
		if err != nil {
			fail(w, 413, err)
			return
		}
		if err = a.Play(r.Context(), data); err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"played": true})
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		a.Control.Stop()
		a.Shutdown()
		writeJSON(w, 200, map[string]bool{"accepted": true})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// No browser-origin access, query-string tokens, or unauthenticated local calls.
		if r.Header.Get("Origin") != "" {
			fail(w, 403, fmt.Errorf("browser origins are not allowed"))
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || a.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) != 1 {
			fail(w, 401, fmt.Errorf("Bearer token required"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *API) action(w http.ResponseWriter, r *http.Request, name string, value int) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := a.Action(ctx, name, value); err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"completed": true})
}

func (a *API) video(w http.ResponseWriter, r *http.Request) {
	if len(a.Media.Frame().Data) == 0 {
		fail(w, 503, fmt.Errorf("no camera frame"))
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=scout-frame")
	t := time.NewTicker(control.Interval)
	defer t.Stop()
	var last time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			f := a.Media.Frame()
			if time.Since(f.At) > 3*time.Second {
				return
			}
			if f.At == last {
				continue
			}
			last = f.At
			_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := fmt.Fprintf(w, "--scout-frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(f.Data)); err != nil {
				return
			}
			if _, err := w.Write(f.Data); err != nil {
				return
			}
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}

func (a *API) microphone(w http.ResponseWriter, r *http.Request) {
	ch, unsub := a.Media.SubscribeAudio()
	defer unsub()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	rc := http.NewResponseController(w)
	started := false
	for {
		select {
		case <-r.Context().Done():
			return
		case <-timeout.C:
			if !started {
				fail(w, 503, fmt.Errorf("no microphone packets"))
			}
			return
		case data := <-ch:
			if !started {
				w.Header().Set("Content-Type", "audio/aac")
				started = true
			}
			_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := w.Write(data); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
			if !timeout.Stop() {
				select {
				case <-timeout.C:
				default:
				}
			}
			timeout.Reset(5 * time.Second)
		}
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || t != "application/json" {
		fail(w, 415, fmt.Errorf("Content-Type must be application/json"))
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		fail(w, 400, err)
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		fail(w, 400, fmt.Errorf("expected exactly one JSON object"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

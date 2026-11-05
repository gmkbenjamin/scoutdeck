package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shell-company/scout/internal/control"
)

func testAPI() *API {
	return &API{Token: "test-secret", Control: &control.Controller{}, Media: &control.Media{}, Status: func() any { return map[string]bool{"ok": true} }, Action: func(context.Context, string, int) error { return errors.New("service rejected request") }, Shutdown: func() {}}
}
func request(a *API, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func TestAuthAndDriveValidation(t *testing.T) {
	a := testAPI()
	for _, tc := range []struct {
		body, token string
		status      int
	}{
		{`{"linear_y":0.2,"ttl_ms":100}`, "", 401},
		{`{"linear_y":0.2,"ttl_ms":100}`, "wrong", 401},
		{`{"linear_y":0.2,"ttl_ms":100}`, a.Token, 200},
		{`{"linear_y":2}`, a.Token, 409},
		{`{"ttl_ms":501}`, a.Token, 409},
		{`{"linear_yy":0.2}`, a.Token, 400},
		{`{} {}`, a.Token, 400},
	} {
		if got := request(a, "POST", "/v1/drive", tc.body, tc.token).Code; got != tc.status {
			t.Fatalf("%s: %d", tc.body, got)
		}
	}
	r := httptest.NewRequest("POST", "/v1/stop", nil)
	r.Header.Set("Authorization", "Bearer "+a.Token)
	r.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	if got := request(a, "GET", "/v1/stop", "", a.Token).Code; got != http.StatusMethodNotAllowed {
		t.Fatal(got)
	}
}

func TestScreenshotAndServiceFailure(t *testing.T) {
	a := testAPI()
	if got := request(a, "GET", "/v1/screenshot.jpg", "", a.Token).Code; got != 503 {
		t.Fatal(got)
	}
	a.Media.PutFrame([]byte{1, 2, 3})
	w := request(a, "GET", "/v1/screenshot.jpg", "", a.Token)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), []byte{1, 2, 3}) {
		t.Fatal(w.Code, w.Body)
	}
	w = request(a, "POST", "/v1/home", "", a.Token)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "service rejected") {
		t.Fatal("service failure hidden", w.Code, w.Body)
	}
}

func testWAV() []byte {
	w := make([]byte, 48)
	copy(w, "RIFF")
	binary.LittleEndian.PutUint32(w[4:], 40)
	copy(w[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(w[16:], 16)
	binary.LittleEndian.PutUint16(w[20:], 1)
	binary.LittleEndian.PutUint16(w[22:], 1)
	binary.LittleEndian.PutUint32(w[24:], 16000)
	binary.LittleEndian.PutUint32(w[28:], 32000)
	binary.LittleEndian.PutUint16(w[32:], 2)
	binary.LittleEndian.PutUint16(w[34:], 16)
	copy(w[36:], "data")
	binary.LittleEndian.PutUint32(w[40:], 4)
	return w
}

func TestWAVValidation(t *testing.T) {
	w := testWAV()
	if pcm, err := PCM16Mono(w); err != nil || len(pcm) != 4 {
		t.Fatal(err)
	}
	for _, index := range []int{0, 4, 8, 20, 22, 24, 28, 32, 34, 40} {
		bad := append([]byte(nil), w...)
		bad[index] ^= 0xff
		if _, err := PCM16Mono(bad); err == nil {
			t.Fatal("accepted corrupt WAV at", index)
		}
	}
	for n := 0; n < len(w); n++ {
		if _, err := PCM16Mono(w[:n]); err == nil {
			t.Fatal("accepted truncated WAV", n)
		}
	}
	for _, dest := range []string{"-oProxyCommand=bad", "root@host;id", "root@$(id)", "root@host\ncommand"} {
		if _, err := NewSSHAudio(dest); err == nil {
			t.Fatal(dest)
		}
	}
}

func TestDockOperationAPI(t *testing.T) {
	a := testAPI()
	a.StartDock = func(seconds int) (any, error) {
		if seconds != 20 {
			t.Errorf("timeout %d", seconds)
		}
		return map[string]any{"state": "starting", "active": true}, nil
	}
	a.DockStatus = func() any { return map[string]string{"state": "searching"} }
	a.Stop = func(context.Context) error { return errors.New("native cancel failed") }
	a.Drive = func(control.Motion, time.Duration) error { return errors.New("docking owns movement") }
	for _, path := range []string{"/v1/dock", "/v1/home"} {
		w := request(a, "POST", path, `{"timeout_seconds":20}`, a.Token)
		if w.Code != 202 || !strings.Contains(w.Body.String(), "starting") {
			t.Fatal(w.Code, w.Body)
		}
	}
	if w := request(a, "POST", "/v1/dock", `{"timeout_seconds":181}`, a.Token); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := request(a, "GET", "/v1/dock", "", a.Token); w.Code != 200 || !strings.Contains(w.Body.String(), "searching") {
		t.Fatal(w.Code, w.Body)
	}
	if w := request(a, "POST", "/v1/drive", `{"linear_y":0.1}`, a.Token); w.Code != 409 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/v1/stop", "/v1/dock/cancel"} {
		if w := request(a, "POST", path, "", a.Token); w.Code != 502 {
			t.Fatal(w.Code)
		}
	}
	a.StartDock = func(int) (any, error) { return nil, errors.New("already active") }
	if w := request(a, "POST", "/v1/dock", `{}`, a.Token); w.Code != 409 {
		t.Fatal(w.Code)
	}
}

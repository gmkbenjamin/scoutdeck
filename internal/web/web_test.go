package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmkbenjamin/scoutdeck/internal/agent"
)

func TestPairingSetsSessionOnlyForTheRightToken(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	h := Handler("secret-token-0123456789abcdef0123456789", nil, api)
	post := func(body string, hdr map[string]string, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/web/pair", strings.NewReader(body))
		if remote != "" {
			r.RemoteAddr = remote
		}
		r.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	same := map[string]string{"Sec-Fetch-Site": "same-origin", "X-Forwarded-Proto": "https"}
	if w := post(`{"token":"wrong"}`, same, "127.0.0.1:1234"); w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatal("wrong token", w.Code)
	}
	if w := post(`{"token":"secret-token-0123456789abcdef0123456789"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}, "127.0.0.1:1234"); w.Code != 403 {
		t.Fatal("cross-site pairing", w.Code)
	}
	if w := post(`{"token":"secret-token-0123456789abcdef0123456789"}`, same, "192.0.2.10:1234"); w.Code != 204 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].Secure {
		t.Fatal("non-loopback peer must not be able to spoof X-Forwarded-Proto")
	}
	w := post(`{"token":"secret-token-0123456789abcdef0123456789"}`, same, "127.0.0.1:1234")
	if w.Code != 204 {
		t.Fatal("pair", w.Code)
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != agent.SessionCookie || c[0].Value != agent.SessionValue("secret-token-0123456789abcdef0123456789") ||
		!c[0].HttpOnly || !c[0].Secure || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", c)
	}
}

func TestStaticAppAndAPIRouting(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	h := Handler("t", nil, api)
	for path, want := range map[string]int{"/": 200, "/app.js": 200, "/app.css": 200, "/manifest.webmanifest": 200, "/v1/status": 299} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s: %d", path, w.Code)
		}
		if path == "/" && !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Error("missing CSP")
		}
	}
}

func TestOneTimeCodePairsOnceAndExpires(t *testing.T) {
	now := time.Now()
	codes := &OneTimeCodes{now: func() time.Time { return now }}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := Handler("long-secret-token-0123456789abcdef", codes, api)
	post := func(code string) int {
		r := httptest.NewRequest("POST", "/web/pair", strings.NewReader(`{"token":"`+code+`"}`))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	code, _ := codes.New()
	if c := post(code[:3] + " " + code[3:]); c != 204 {
		t.Fatal("code with a space", c)
	}
	if c := post(code); c != 401 {
		t.Fatal("code reused", c)
	}
	code, _ = codes.New()
	now = now.Add(CodeLifetime + time.Second)
	if c := post(code); c != 401 {
		t.Fatal("expired code accepted", c)
	}
	now = time.Now()
	codes.now = func() time.Time { return now }
	code, _ = codes.New()
	for i := 0; i < codeMaxFails; i++ {
		post("000000")
		if code == "000000" {
			t.Skip("unlucky code")
		}
	}
	if c := post(code); c != 401 {
		t.Fatal("code survived too many wrong guesses", c)
	}
	if c := post("long-secret-token-0123456789abcdef"); c != 204 {
		t.Fatal("full token stopped working", c)
	}
}

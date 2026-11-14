// Package web serves the browser HUD and pairs browsers with the agent API.
package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/shell-company/scout/internal/agent"
)

//go:embed static
var files embed.FS

// sessionAge keeps a paired phone or tablet signed in for 90 days.
const sessionAge = 90 * 24 * time.Hour

// Handler serves the web app at / and passes /v1/ to api. Static files need
// no authentication; the app pairs by exchanging the API token for a session
// cookie.
func Handler(token string, codes *OneTimeCodes, api http.Handler) http.Handler {
	static, _ := fs.Sub(files, "static")
	assets := http.FileServer(http.FS(static))
	mux := http.NewServeMux()
	mux.Handle("/v1/", api)
	mux.HandleFunc("POST /web/pair", func(w http.ResponseWriter, r *http.Request) { pair(w, r, token, codes) })
	mux.HandleFunc("POST /web/unpair", func(w http.ResponseWriter, r *http.Request) {
		if !agent.SameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: agent.SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r)})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html") || strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
			h.Set("Cache-Control", "no-cache")
		}
		if strings.HasSuffix(r.URL.Path, ".webmanifest") {
			h.Set("Content-Type", "application/manifest+json")
		}
		assets.ServeHTTP(w, r)
	})
	return mux
}

func secure(r *http.Request) bool {
	return r.TLS != nil || (agent.IsLoopback(r.RemoteAddr) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

func pair(w http.ResponseWriter, r *http.Request, token string, codes *OneTimeCodes) {
	w.Header().Set("Cache-Control", "no-store")
	if !agent.SameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		http.Error(w, "expected JSON {\"token\": ...}", http.StatusBadRequest)
		return
	}
	guess := strings.Join(strings.Fields(in.Token), "") // "123 456" works too
	okToken := token != "" && subtle.ConstantTimeCompare([]byte(guess), []byte(token)) == 1
	okCode := !okToken && codes != nil && codes.Take(guess)
	if !okToken && !okCode {
		// Slow down guessing; the token itself is 256 bits.
		time.Sleep(700 * time.Millisecond)
		http.Error(w, "that code is not valid", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: agent.SessionCookie, Value: agent.SessionValue(token), Path: "/",
		MaxAge: int(sessionAge / time.Second), HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

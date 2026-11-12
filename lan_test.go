package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLANListenerKeepsAgentAPILocal(t *testing.T) {
	h := lanOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }))
	for _, c := range []struct {
		path, auth string
		want       int
	}{
		{"/", "", 299},
		{"/v1/status", "", 299}, // cookie-authenticated web app
		{"/v1/status", "Bearer x", 403},
		{"/v1/shutdown", "", 403},
	} {
		r := httptest.NewRequest("GET", c.path, nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s auth=%q: %d, want %d", c.path, c.auth, w.Code, c.want)
		}
	}
	for _, ip := range privateIPv4s() {
		t.Logf("private address: %s", ip)
	}
}

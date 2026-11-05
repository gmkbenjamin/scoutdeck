package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientAuthAndFailures(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing token")
		}
		if r.URL.Path == "/v1/dock" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"already active"}`))
			return
		}
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong request")
		}
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	defer s.Close()
	c := client{base: s.URL, token: "test", http: s.Client()}
	if _, err := c.json(context.Background(), "POST", "/v1/stop", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.json(context.Background(), "POST", "/v1/dock", nil); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatal(err)
	}
}

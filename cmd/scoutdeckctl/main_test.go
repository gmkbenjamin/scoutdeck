package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvAndTokenFallback(t *testing.T) {
	t.Setenv("SCOUT_API_URL", "http://127.0.0.1:9999")
	if got := envFirst("http://127.0.0.1:8765", "SCOUTDECK_API_URL", "SCOUT_API_URL"); got != "http://127.0.0.1:9999" {
		t.Fatalf("url fallback = %q", got)
	}
	t.Setenv("SCOUTDECK_API_URL", "http://127.0.0.1:8888")
	if got := envFirst("http://127.0.0.1:8765", "SCOUTDECK_API_URL", "SCOUT_API_URL"); got != "http://127.0.0.1:8888" {
		t.Fatalf("url primary = %q", got)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("SCOUTDECK_API_TOKEN", "")
	t.Setenv("SCOUT_API_TOKEN", "")
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg, "Scout"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "Scout", "api-token"), []byte("legacy-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveAPIToken(); got != "legacy-file-token" {
		t.Fatalf("legacy file token = %q", got)
	}
	if err := os.MkdirAll(filepath.Join(cfg, "ScoutDeck"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "ScoutDeck", "api-token"), []byte("primary-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveAPIToken(); got != "primary-file-token" {
		t.Fatalf("primary file token = %q", got)
	}
}

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

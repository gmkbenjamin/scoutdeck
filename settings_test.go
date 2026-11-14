package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSaveSettingsConcurrentPreservesLatestValidJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	a := &application{settings: settings{Host: "192.168.1.50"}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				a.settingsMu.Lock()
				switch (id + j) % 3 {
				case 0:
					a.settings.AutoDock = j%2 == 0
				case 1:
					a.settings.LANAccess = j%2 == 1
				case 2:
					a.settings.Guard.Armed = j%2 == 0
				}
				a.settingsMu.Unlock()
				if err := a.saveSettings(); err != nil {
					t.Errorf("saveSettings: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	path, err := settingsPath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var loaded settings
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("saved settings invalid JSON: %v\n%s", err, raw)
	}
	if loaded.Host != "192.168.1.50" {
		t.Fatalf("Host = %q, want 192.168.1.50", loaded.Host)
	}
	a.settingsMu.Lock()
	want := a.settings
	a.settingsMu.Unlock()
	if loaded != want {
		t.Fatalf("loaded settings = %+v, want %+v", loaded, want)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Fatalf("unexpected leftover file in settings dir: %s", e.Name())
		}
	}
}

func TestConfigMigratesFromLegacyScoutAndTokenFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("SCOUTDECK_API_TOKEN", "")
	t.Setenv("SCOUT_API_TOKEN", "")
	cfgRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}

	legacyDir := filepath.Join(cfgRoot, "Scout")
	if err := os.MkdirAll(filepath.Join(legacyDir, "routes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "settings.json"), []byte(`{"host":"192.0.2.42"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyTok := "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(legacyDir, "api-token"), []byte(legacyTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "routes", "kitchen.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := loadSettings()
	if s.Host != "192.0.2.42" {
		t.Fatalf("migrated host = %q, want 192.0.2.42", s.Host)
	}
	tok, src, err := apiToken()
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(cfgRoot, "ScoutDeck", "api-token")
	if tok != legacyTok || src != wantPath {
		t.Fatalf("apiToken = (%q, %q), want (%q, %q)", tok, src, legacyTok, wantPath)
	}
	if _, err := os.Stat(filepath.Join(cfgRoot, "ScoutDeck", "routes", "kitchen.json")); err != nil {
		t.Fatalf("routes dir not migrated: %v", err)
	}
	if _, err := os.Stat(legacyDir); !os.IsNotExist(err) {
		t.Fatalf("expected legacy dir moved away, stat err = %v", err)
	}

	t.Setenv("SCOUT_API_TOKEN", "legacy-env-token-0123456789abcdef")
	if got, src, err := apiToken(); err != nil || got != "legacy-env-token-0123456789abcdef" || src != "$SCOUT_API_TOKEN" {
		t.Fatalf("legacy env fallback = (%q, %q, %v)", got, src, err)
	}
	t.Setenv("SCOUTDECK_API_TOKEN", "primary-env-token-0123456789abcdef")
	if got, src, err := apiToken(); err != nil || got != "primary-env-token-0123456789abcdef" || src != "$SCOUTDECK_API_TOKEN" {
		t.Fatalf("primary env override = (%q, %q, %v)", got, src, err)
	}
}

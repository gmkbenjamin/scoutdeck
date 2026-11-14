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

package main

import "testing"

func TestOfflineOK(t *testing.T) {
	for path, ok := range map[string]bool{
		"/v1/status": true, "/v1/events": true, "/v1/events/0123456789abcdef.jpg": true, "/v1/guard": true, "/v1/routes": true,
		"/v1/move": false, "/v1/drive": false, "/v1/camera": false, "/v1/record/start": false,
	} {
		if offlineOK(path) != ok {
			t.Errorf("%s: want %v", path, ok)
		}
	}
}

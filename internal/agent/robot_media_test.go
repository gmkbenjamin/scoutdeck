package agent

import "testing"

func TestValidMediaPath(t *testing.T) {
	for path, ok := range map[string]bool{
		"/userdata/roller_eye/media_files/2026-02-09-18-51-30_abc-100106.mp4": true,
		"/userdata/roller_eye/media_files/../config/sn":                       false,
		"/userdata/roller_eye/media_files/a/b.mp4":                            false,
		"/etc/passwd": false,
		"/userdata/roller_eye/media_files/x;reboot": false,
		"/userdata/roller_eye/media_files/":         false,
	} {
		if ValidMediaPath(path) != ok {
			t.Errorf("%q: want %v", path, ok)
		}
	}
}

func TestParseHealth(t *testing.T) {
	h := parseHealth("firmware=01_HW32\nuptime=100.5\ntemp=69230\ntemp=18800\nbattery_voltage=8308000\nbattery_current=\nwifi_signal=-39\nwifi_ssid=Home\n")
	if h["firmware"] != "01_HW32" || h["uptime"] != 100.5 || h["temperature_c"] != 69.23 || h["battery_volts"] != 8.308 || h["wifi_signal"] != -39.0 || h["wifi_ssid"] != "Home" {
		t.Fatalf("%v", h)
	}
	if _, ok := h["battery_amps"]; ok {
		t.Fatal("empty values must be skipped")
	}
}

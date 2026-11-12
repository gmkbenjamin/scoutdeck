package main

import (
	"testing"

	"github.com/aler9/goroslib/pkg/serviceproc"
)

// Checksums reported by `rossrv md5 roller_eye/<name>` on the robot. A
// mismatch means the robot would refuse the call.
func TestWiFiServiceChecksumsMatchRobot(t *testing.T) {
	for _, c := range []struct {
		srv  any
		want string
	}{
		{wifiScanService{}, "fd54a28d4e6325d269f60a72dc202a7b"},
		{wifiSwitchService{}, "879dd501d4c8e7d6703c458f6fc7737c"},
	} {
		got, err := serviceproc.MD5(c.srv)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%T md5 %s, robot expects %s", c.srv, got, c.want)
		}
	}
}

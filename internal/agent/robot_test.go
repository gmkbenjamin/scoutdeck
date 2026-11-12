package agent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBCBScriptSetsRecoveryCommandOnly(t *testing.T) {
	misc := filepath.Join(t.TempDir(), "misc.img")
	img := make([]byte, 4<<20)
	copy(img[0x4344:], "/userdata/ota/update.img") // left by a previous recovery
	copy(img[0x4000:], "stale-command")
	copy(img[0x4020:], "stale-status")
	if err := os.WriteFile(misc, img, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", writeBCBScript)
	cmd.Env = append(os.Environ(), "MISC="+misc)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "written") {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := os.ReadFile(misc)
	if len(got) != len(img) {
		t.Fatalf("size changed: %d", len(got))
	}
	field := func(off, n int) string { return string(bytes.TrimRight(got[off:off+n], "\x00")) }
	if field(0x4000, 32) != "boot-recovery" || field(0x4020, 32) != "" || field(0x4040, 768) != "recovery\n" {
		t.Fatalf("message: %q %q %q", field(0x4000, 32), field(0x4020, 32), field(0x4040, 768))
	}
	if field(0x4344, 32) != "/userdata/ota/update.img" {
		t.Fatal("bytes outside the message changed")
	}
	got[0x4000], got[0x4040] = 0, 0
	for i := range got {
		if (i < 0x4000 || i >= 0x4000+832) && got[i] != img[i] {
			t.Fatalf("byte %#x changed", i)
		}
	}
}

func TestRestoreChecks(t *testing.T) {
	out := "image:update.tar.xz|1|present\nimage:backup.sh|1|present\nimage:recovery_status|1|present\nota|1|none\nmisc|1|empty\npower|1|on the charger (Charging)\nbattery|1|80%\n"
	checks := parseChecks(out)
	if len(checks) != 7 || !RestoreReady(checks) {
		t.Fatal(checks)
	}
	checks[5].OK = false
	if RestoreReady(checks) {
		t.Fatal("ready with a failed check")
	}
	if RestoreReady(parseChecks("misc|1|empty")) {
		t.Fatal("ready with missing checks")
	}
}

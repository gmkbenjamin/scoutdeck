package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Robot maintenance over SSH. Every remote command below is a fixed string;
// user-supplied values travel only on stdin, never through a shell.

// WiFiRevertAfter is how long the robot gets to join a new network before the
// on-robot watchdog restores the previous configuration and reboots.
const WiFiRevertAfter = 3 * time.Minute

const hotspotScript = `printf 'robot-scout-%s' "$(md5sum /var/roller_eye/config/sn | cut -c27-32)"`

// HotspotSSID returns the network name the robot uses in hotspot mode after a
// settings reset. The firmware derives it from the serial number; the
// password is the firmware default r0123456.
func (a *SSHAudio) HotspotSSID(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	out, err := a.command(ctx, hotspotScript).Output()
	ssid := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(ssid, "robot-scout-") || len(ssid) != len("robot-scout-")+6 {
		return "", fmt.Errorf("cannot read the robot's hotspot name: %v", err)
	}
	return ssid, nil
}

// The firmware's own settings reset: clears Wi-Fi, cloud binding, timezone,
// maps, patrols and recordings, restores default settings and reboots into
// hotspot mode. The OS, SSH keys and root password are untouched. It is
// detached because it stops the ROS services this session is using.
const resetScript = `R=/usr/local/bin/scout_reset2.sh
test -x $R || { echo "$R not found"; exit 1; }
setsid $R </dev/null >/dev/null 2>&1 &
echo started`

func (a *SSHAudio) ResetSettings(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	out, err := a.command(ctx, resetScript).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("started")) {
		return fmt.Errorf("settings reset did not start: %v %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// stageWiFiScript saves the credentials the firmware reads at boot
// (/var/roller_eye/config/wifi: SSID line, key line) and arms a watchdog. If
// the robot is not associated with an IPv4 address after WiFiRevertAfter, the
// watchdog restores the previous file (or removes it, which means hotspot
// mode) and reboots, so a mistyped password cannot strand the robot.
const stageWiFiScript = `set -e
C=/var/roller_eye/config
rm -f $C/wifi.scout-prev $C/wifi.scout-tmp
if [ -f $C/wifi ]; then cp -p $C/wifi $C/wifi.scout-prev; fi
cat > $C/wifi.scout-tmp
chmod 644 $C/wifi.scout-tmp
mv -f $C/wifi.scout-tmp $C/wifi
sync
P=/tmp/scout-wifi-watchdog.pid
if [ -f $P ]; then kill "$(cat $P)" 2>/dev/null || true; fi
setsid sh -c '
C=/var/roller_eye/config
echo $$ > /tmp/scout-wifi-watchdog.pid
sleep 180
rm -f /tmp/scout-wifi-watchdog.pid
if wpa_cli -i wlan0 status 2>/dev/null | grep -q "^wpa_state=COMPLETED" && ip -4 addr show wlan0 | grep -q "inet "; then
	rm -f $C/wifi.scout-prev
	exit 0
fi
if [ -f $C/wifi.scout-prev ]; then mv -f $C/wifi.scout-prev $C/wifi; else rm -f $C/wifi; fi
sync
reboot
' scout-wifi-watchdog </dev/null >/dev/null 2>&1 &
echo staged`

// ValidateWiFi checks credentials before anything is sent to the robot.
func ValidateWiFi(ssid, key string) error {
	if ssid == "" || len(ssid) > 32 {
		return fmt.Errorf("network name must be 1-32 bytes")
	}
	if key != "" && (len(key) < 8 || len(key) > 63) {
		return fmt.Errorf("WPA passwords are 8-63 characters (leave empty for an open network)")
	}
	if strings.ContainsAny(ssid+key, "\n\r\x00") {
		return fmt.Errorf("network name and password cannot contain line breaks")
	}
	return nil
}

// StageWiFi persists the new credentials and arms the revert watchdog. The
// caller then asks the running firmware to switch networks.
func (a *SSHAudio) StageWiFi(parent context.Context, ssid, key string) error {
	if err := ValidateWiFi(ssid, key); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	cmd := a.command(ctx, stageWiFiScript)
	cmd.Stdin = strings.NewReader(ssid + "\n" + key + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("staged")) {
		return fmt.Errorf("could not save Wi-Fi settings on the robot: %v %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// Factory restore reinstalls the root filesystem from the factory image on the
// robot's backup partition. Booting the recovery partition runs
// /etc/init.d/S41_backup, which runs backup.sh: it reformats rootfs and writes
// update/rootfs.img (unpacked from update.tar.xz) unless /userdata/ota/update.img
// exists. Recovery is entered through the Android/Rockchip bootloader message
// ("boot-recovery") at 16 KiB into the misc partition.
//
// Everything on rootfs returns to the factory state, including SSH
// authorized_keys, the root password, Wi-Fi settings and the serial-derived
// config the firmware recreates on first boot. /userdata is not wiped.

// RestoreCheck is one precondition for a factory restore.
type RestoreCheck struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Info string `json:"info"`
}

// restorePreflightScript only reads. Each line is "name|ok|info".
const restorePreflightScript = `MISC=$(readlink -f /dev/disk/by-partlabel/misc)
BACKUP=$(readlink -f /dev/disk/by-partlabel/backup)
ls_backup=$(debugfs -R "ls -l /" "$BACKUP" 2>/dev/null)
for f in update.tar.xz backup.sh recovery_status; do
	if printf '%s\n' "$ls_backup" | grep -q " $f\$"; then echo "image:$f|1|present"; else echo "image:$f|0|missing from the backup partition"; fi
done
if [ -e /userdata/ota/update.img ]; then echo "ota|0|/userdata/ota/update.img exists; recovery would install it instead"; else echo "ota|1|no pending OTA image"; fi
if [ -b "$MISC" ]; then
	cmd=$(dd if="$MISC" bs=1 skip=16384 count=32 2>/dev/null | tr -d '\000')
	if [ -z "$cmd" ]; then echo "misc|1|bootloader message is empty"; else echo "misc|0|bootloader message already set: $cmd"; fi
else
	echo "misc|0|misc partition not found"
fi
st=$(cat /sys/class/power_supply/rk-bat/status 2>/dev/null)
cap=$(cat /sys/class/power_supply/rk-bat/capacity 2>/dev/null)
case "$st" in Charging|Full|"Not charging") echo "power|1|on the charger ($st)";; *) echo "power|0|not on the charger ($st); dock the robot first";; esac
if [ -n "$cap" ] && [ "$cap" -ge 50 ]; then echo "battery|1|$cap%"; else echo "battery|0|$cap% (needs at least 50%)"; fi`

// RestorePreflight reports whether a factory restore can start. It changes nothing.
func (a *SSHAudio) RestorePreflight(parent context.Context) ([]RestoreCheck, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	out, err := a.command(ctx, restorePreflightScript).Output()
	if err != nil {
		return nil, fmt.Errorf("restore checks failed: %w", err)
	}
	checks := parseChecks(string(out))
	if len(checks) < 7 {
		return checks, fmt.Errorf("restore checks were incomplete")
	}
	return checks, nil
}

func parseChecks(out string) []RestoreCheck {
	var checks []RestoreCheck
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) == 3 {
			checks = append(checks, RestoreCheck{Name: parts[0], OK: parts[1] == "1", Info: parts[2]})
		}
	}
	return checks
}

// RestoreReady reports whether every check passed.
func RestoreReady(checks []RestoreCheck) bool {
	if len(checks) < 7 {
		return false
	}
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// writeBCBScript sets the bootloader message and verifies it. $MISC is the
// misc partition; tests point it at a file.
const writeBCBScript = `set -e
off=16384
# command[32] + status[32] + recovery[768]
dd if=/dev/zero of="$MISC" bs=1 seek=$off count=832 conv=notrunc 2>/dev/null
printf 'boot-recovery' | dd of="$MISC" bs=1 seek=$off conv=notrunc 2>/dev/null
printf 'recovery\n' | dd of="$MISC" bs=1 seek=$((off + 64)) conv=notrunc 2>/dev/null
sync
got=$(dd if="$MISC" bs=1 skip=$off count=32 2>/dev/null | tr -d '\000')
test "$got" = boot-recovery
echo written`

// startRestoreScript re-checks the preconditions on the robot, writes the
// bootloader message and reboots into recovery.
const startRestoreScript = `MISC=$(readlink -f /dev/disk/by-partlabel/misc)
test -b "$MISC" || { echo "misc partition not found"; exit 1; }
test ! -e /userdata/ota/update.img || { echo "pending OTA image"; exit 1; }
` + writeBCBScript + `
setsid sh -c 'sleep 2; sync; reboot' </dev/null >/dev/null 2>&1 &`

// StartFactoryRestore reboots the robot into recovery to reinstall its
// system. Callers must run RestorePreflight first and require confirmation.
func (a *SSHAudio) StartFactoryRestore(parent context.Context) error {
	checks, err := a.RestorePreflight(parent)
	if err != nil {
		return err
	}
	if !RestoreReady(checks) {
		return fmt.Errorf("factory restore preconditions are no longer met")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	out, err := a.command(ctx, startRestoreScript).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("written")) {
		return fmt.Errorf("could not start factory restore: %v %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// healthScript only reads. Each line is "key=value".
const healthScript = `echo "firmware=$(cat /var/roller_eye/config/version 2>/dev/null)"
echo "uptime=$(cut -d' ' -f1 /proc/uptime)"
echo "load=$(cut -d' ' -f1-3 /proc/loadavg)"
for z in /sys/class/thermal/thermal_zone*/temp; do [ -r "$z" ] && echo "temp=$(cat $z)"; done
echo "battery_status=$(cat /sys/class/power_supply/rk-bat/status 2>/dev/null)"
echo "battery_capacity=$(cat /sys/class/power_supply/rk-bat/capacity 2>/dev/null)"
echo "battery_voltage=$(cat /sys/class/power_supply/rk-bat/voltage_now 2>/dev/null)"
echo "battery_current=$(cat /sys/class/power_supply/rk-bat/current_now 2>/dev/null)"
w=$(wpa_cli -i wlan0 status 2>/dev/null)
echo "wifi_ssid=$(printf '%s\n' "$w" | sed -n 's/^ssid=//p')"
echo "wifi_ip=$(printf '%s\n' "$w" | sed -n 's/^ip_address=//p')"
echo "wifi_freq=$(printf '%s\n' "$w" | sed -n 's/^freq=//p')"
echo "wifi_signal=$(awk 'NR==3{gsub("\\.","",$4); print $4}' /proc/net/wireless 2>/dev/null)"
df -k /userdata 2>/dev/null | awk 'NR==2{print "storage_used_kb="$3; print "storage_total_kb="$2}'
free -k 2>/dev/null | awk '/Mem:/{print "memory_used_kb="$3; print "memory_total_kb="$2}'`

// Health reads the robot's vital signs over SSH. It changes nothing.
func (a *SSHAudio) Health(parent context.Context) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	out, err := a.command(ctx, healthScript).Output()
	if err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}
	return parseHealth(string(out)), nil
}

func parseHealth(out string) map[string]any {
	h := map[string]any{}
	var temps []float64
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || v == "" {
			continue
		}
		num := func() (float64, bool) {
			f, err := strconv.ParseFloat(v, 64)
			return f, err == nil
		}
		switch k {
		case "temp":
			if f, ok := num(); ok {
				temps = append(temps, f/1000)
			}
		case "uptime", "battery_capacity", "wifi_freq", "wifi_signal", "storage_used_kb", "storage_total_kb", "memory_used_kb", "memory_total_kb":
			if f, ok := num(); ok {
				h[k] = f
			}
		case "battery_voltage":
			if f, ok := num(); ok {
				h["battery_volts"] = f / 1e6
			}
		case "battery_current":
			if f, ok := num(); ok {
				h["battery_amps"] = f / 1e6
			}
		default:
			h[k] = v
		}
	}
	if len(temps) > 0 {
		hot := temps[0]
		for _, t := range temps {
			hot = max(hot, t)
		}
		h["temperature_c"] = hot
	}
	return h
}

// MediaDir is where the firmware keeps its recordings and snapshots.
const MediaDir = "/userdata/roller_eye/media_files/"

var mediaPath = regexp.MustCompile(`^/userdata/roller_eye/media_files/[A-Za-z0-9_.-]{1,160}$`)

// ValidMediaPath reports whether path is a plain file in the firmware's media folder.
func ValidMediaPath(path string) bool {
	return mediaPath.MatchString(path) && !strings.Contains(path, "..")
}

// readMediaScript reads the file named on stdin after checking it again on the robot.
const readMediaScript = `IFS= read -r p
case "$p" in /userdata/roller_eye/media_files/*) ;; *) exit 3;; esac
case "$p" in *..*|*/*/*/*/*/*) exit 3;; esac
test -f "$p" || exit 4
exec cat -- "$p"`

// ReadMedia streams one firmware recording to w. The path travels on stdin.
func (a *SSHAudio) ReadMedia(ctx context.Context, path string, w io.Writer) error {
	if !ValidMediaPath(path) {
		return fmt.Errorf("not a recording path")
	}
	cmd := a.command(ctx, readMediaScript)
	cmd.Stdin = strings.NewReader(path + "\n")
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not read the recording: %v %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

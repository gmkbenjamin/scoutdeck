package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shell-company/scout/internal/docking"
)

type SSHAudio struct {
	destination string
	speaker     sync.Mutex
}

func NewSSHAudio(destination string) (*SSHAudio, error) {
	// This is a destination only, never a shell command or an SSH option.
	if !regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*@[a-zA-Z0-9][a-zA-Z0-9.-]*$`).MatchString(destination) {
		return nil, fmt.Errorf("-ssh must be user@hostname or user@IPv4")
	}
	return &SSHAudio{destination: destination}, nil
}

func (a *SSHAudio) command(ctx context.Context, command string) *exec.Cmd {
	// Use the user's ordinary SSH agent/configuration; never open credential files.
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=accept-new"}
	if runtime.GOOS != "windows" {
		// The robot takes 2-3 s per SSH handshake; reuse one connection so the
		// once-per-second charging probe stays well inside its deadline.
		// The socket lives in the user's private ~/.ssh, not a shared temp dir.
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath=~/.ssh/scout-%C", "-o", "ControlPersist=60")
	}
	cmd := exec.CommandContext(ctx, "ssh", append(args, a.destination, command)...)
	cmd.WaitDelay = time.Second
	return cmd
}

func (a *SSHAudio) Power(parent context.Context) (docking.Power, error) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	data, err := a.command(ctx, "cat /sys/class/power_supply/rk-bat/status").Output()
	if err != nil {
		return docking.Power{}, fmt.Errorf("charging status unavailable: %w", err)
	}
	p := docking.Power{Status: strings.TrimSpace(string(data)), ObservedAt: time.Now()}
	switch p.Status {
	// "Not charging" means the charger is connected but not charging, e.g.
	// when the battery is nearly full on the dock.
	case "Charging", "Full", "Not charging":
		p.Connected = true
	case "Discharging":
	default:
		return docking.Power{}, fmt.Errorf("unknown charging status %q", p.Status)
	}
	return p, nil
}

func (a *SSHAudio) Play(parent context.Context, data []byte) error {
	pcm, err := PCM16Mono(data)
	if err != nil {
		return err
	}
	if !a.speaker.TryLock() {
		return fmt.Errorf("speaker is busy")
	}
	defer a.speaker.Unlock()
	ctx, cancel := context.WithTimeout(parent, 36*time.Second)
	defer cancel()
	cmd := a.command(ctx, "timeout 32 aplay -q -t raw -f S16_LE -r 16000 -c 1")
	cmd.Stdin = bytes.NewReader(pcm)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("speaker playback failed: %w: %s", err, out)
	}
	return nil
}

// PCM16Mono accepts only bounded, uncompressed WAV; no decoder or shell input is supplied by callers.
func PCM16Mono(wav []byte) ([]byte, error) {
	bad := fmt.Errorf("expected a PCM WAV: mono, 16000 Hz, 16-bit little-endian, at most 30 seconds")
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(wav[4:8]))+8 != uint64(len(wav)) {
		return nil, bad
	}
	valid := false
	var pcm []byte
	for pos := 12; pos < len(wav); {
		if len(wav)-pos < 8 {
			return nil, bad
		}
		n := uint64(binary.LittleEndian.Uint32(wav[pos+4 : pos+8]))
		start := pos + 8
		if n > uint64(len(wav)-start) {
			return nil, bad
		}
		chunk := wav[start : start+int(n)]
		switch string(wav[pos : pos+4]) {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, bad
			}
			valid = binary.LittleEndian.Uint16(chunk[:2]) == 1 && binary.LittleEndian.Uint16(chunk[2:4]) == 1 && binary.LittleEndian.Uint32(chunk[4:8]) == 16000 && binary.LittleEndian.Uint32(chunk[8:12]) == 32000 && binary.LittleEndian.Uint16(chunk[12:14]) == 2 && binary.LittleEndian.Uint16(chunk[14:16]) == 16
			if !valid {
				return nil, bad
			}
		case "data":
			if pcm != nil {
				return nil, bad
			}
			pcm = chunk
		}
		pos = start + int(n) + int(n%2)
		if pos > len(wav) {
			return nil, bad
		}
	}
	if !valid || len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > 30*32000 {
		return nil, bad
	}
	return pcm, nil
}

// Battery reads the battery driver's status and charge in one SSH call.
func (a *SSHAudio) Battery(parent context.Context) (status string, percent int, err error) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	out, err := a.command(ctx, "cat /sys/class/power_supply/rk-bat/status /sys/class/power_supply/rk-bat/capacity").Output()
	if err != nil {
		return "", -1, fmt.Errorf("battery status unavailable: %w", err)
	}
	lines := strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(out)), "Not charging", "Not_charging"))
	if len(lines) != 2 {
		return "", -1, fmt.Errorf("unexpected battery status %q", out)
	}
	pct, err := strconv.Atoi(lines[1])
	if err != nil || pct < 0 || pct > 100 {
		return "", -1, fmt.Errorf("unexpected battery level %q", lines[1])
	}
	return strings.ReplaceAll(lines[0], "_", " "), pct, nil
}

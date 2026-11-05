package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"regexp"
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
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=yes", a.destination, command)
	cmd.WaitDelay = time.Second
	return cmd
}

func (a *SSHAudio) Power(parent context.Context) (docking.Power, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	data, err := a.command(ctx, "cat /sys/class/power_supply/rk-bat/status").Output()
	if err != nil {
		return docking.Power{}, fmt.Errorf("charging status unavailable: %w", err)
	}
	p := docking.Power{Status: strings.TrimSpace(string(data)), ObservedAt: time.Now()}
	switch p.Status {
	case "Charging", "Full":
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

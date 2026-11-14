package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/gmkbenjamin/scoutdeck/internal/control"
)

type Microphone struct {
	Media *control.Media
	mu    sync.Mutex
}

// Record uses the robot's existing AAC publication rather than competing for ALSA.
func (m *Microphone) Record(parent context.Context, seconds int) ([]byte, error) {
	if seconds < 1 || seconds > 10 {
		return nil, fmt.Errorf("seconds must be 1..10")
	}
	if !m.mu.TryLock() {
		return nil, fmt.Errorf("microphone recording is busy")
	}
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, time.Duration(seconds+6)*time.Second)
	defer cancel()
	ch, unsub := m.Media.SubscribeAudio()
	defer unsub()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	var encoded bytes.Buffer
	started := false
capture:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			if !started {
				return nil, fmt.Errorf("no microphone packets received")
			}
			break capture
		case data := <-ch:
			if !started {
				started = true
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(time.Duration(seconds) * time.Second)
			}
			if encoded.Len()+len(data) > 1<<20 {
				return nil, fmt.Errorf("microphone data exceeds recording limit")
			}
			encoded.Write(data)
		}
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "aac", "-i", "pipe:0", "-t", strconv.Itoa(seconds), "-ac", "1", "-ar", "16000", "-f", "s16le", "pipe:1")
	cmd.Stdin = &encoded
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pcm, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("AAC to WAV conversion failed (ffmpeg required): %w: %s", err, stderr.String())
	}
	if len(pcm) == 0 || len(pcm) > seconds*32000 {
		return nil, fmt.Errorf("invalid decoded microphone length")
	}
	wav := make([]byte, 44+len(pcm))
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return wav, nil
}

// StreamPCM decodes the robot's AAC microphone stream to raw 16 kHz mono
// signed 16-bit little-endian PCM for speech recognition. It ends after five
// seconds without microphone packets or when the client disconnects.
func (a *API) microphonePCM(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ch, unsub := a.Media.SubscribeAudio()
	defer unsub()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-fflags", "nobuffer", "-probesize", "32", "-analyzeduration", "0",
		"-f", "aac", "-i", "pipe:0", "-ac", "1", "-ar", "16000", "-f", "s16le", "pipe:1")
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fail(w, 500, err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fail(w, 500, err)
		return
	}
	if err := cmd.Start(); err != nil {
		fail(w, 503, fmt.Errorf("ffmpeg is required for PCM microphone audio: %w", err))
		return
	}
	defer cmd.Wait()
	go func() {
		defer stdin.Close()
		idle := time.NewTimer(5 * time.Second)
		defer idle.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-idle.C:
				cancel()
				return
			case data := <-ch:
				if _, err := stdin.Write(data); err != nil {
					return
				}
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(5 * time.Second)
			}
		}
	}()
	w.Header().Set("Content-Type", "audio/L16;rate=16000;channels=1")
	rc := http.NewResponseController(w)
	buf := make([]byte, 3200) // 100 ms
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
)

var flagVoice = flag.Bool("voice", false, "start the on-device voice assistant once connected (toggle with V)")

// voiceAssistant runs scout-voice, the on-device (Apple Intelligence) voice
// helper, and turns its JSON events into HUD state and notices.
type voiceAssistant struct {
	mu    sync.Mutex
	cmd   *exec.Cmd
	state string // "", "starting", "listening", "awake", "thinking", "speaking"
}

func voiceBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(exe), "scout-voice")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("voice helper not found next to the app (%s); build it with scripts/package.py or swiftc voice/main.swift", path)
	}
	return path, nil
}

func (v *voiceAssistant) State() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

func (v *voiceAssistant) setState(s string) {
	v.mu.Lock()
	v.state = s
	v.mu.Unlock()
}

// toggle starts or stops the assistant.
func (a *application) toggleVoice() {
	v := &a.voice
	v.mu.Lock()
	if v.cmd != nil {
		_ = v.cmd.Process.Signal(syscall.SIGTERM)
		v.mu.Unlock()
		a.notices.add("Voice assistant off", noticeInfo)
		return
	}
	v.mu.Unlock()
	if a.apiToken == "" {
		a.notices.add("Voice needs the local API (started with -api off?)", noticeBad)
		return
	}
	if a.audio == nil {
		a.notices.add("Voice needs SSH for the robot's speaker", noticeBad)
		return
	}
	path, err := voiceBinary()
	if err != nil {
		a.notices.add(err.Error(), noticeBad)
		return
	}
	cmd := exec.CommandContext(a.ctx, path)
	// The token goes through the environment, never the command line.
	cmd.Env = append(os.Environ(), "SCOUT_API_URL=http://"+*flagAPI, "SCOUT_API_TOKEN="+a.apiToken)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		a.notices.add(err.Error(), noticeBad)
		return
	}
	if err := cmd.Start(); err != nil {
		a.notices.add("Voice assistant: "+err.Error(), noticeBad)
		return
	}
	v.mu.Lock()
	v.cmd, v.state = cmd, "starting"
	v.mu.Unlock()
	a.notices.add(`Voice assistant on - say "Scout, …"`, noticeGood)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			var ev struct{ Event, Text string }
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			switch ev.Event {
			case "listening", "thinking":
				v.setState(ev.Event)
			case "wake":
				v.setState("awake")
				a.notices.add("Listening…", noticeInfo)
			case "heard":
				log.Printf("voice heard: %q", ev.Text)
				a.notices.add(`Heard: "`+ev.Text+`"`, noticeInfo)
			case "action":
				log.Printf("voice action: %s", ev.Text)
				a.notices.add("Doing: "+ev.Text, noticeInfo)
			case "speaking":
				v.setState("speaking")
				log.Printf("voice says: %q", ev.Text)
				a.notices.add("Scout: "+ev.Text, noticeGood)
			case "status":
				a.notices.add(ev.Text, noticeInfo)
			case "error", "fatal":
				log.Printf("voice: %s", ev.Text)
				a.notices.add("Voice: "+ev.Text, noticeBad)
			}
		}
		err := cmd.Wait()
		if err != nil && a.ctx.Err() == nil {
			log.Printf("voice helper exited: %v", err)
		}
		v.mu.Lock()
		v.cmd, v.state = nil, ""
		v.mu.Unlock()
	}()
}

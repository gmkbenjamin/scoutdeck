package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var flagHost = flag.String("host", "", "robot hostname or IP; saved for next time (ROS on :11311, SSH as root@host)")
var flagNoSSH = flag.Bool("no-ssh", false, "disable SSH (speaker playback and verified docking)")

// settings holds non-secret device details. Credentials stay in the user's SSH
// agent and keys; nothing here grants access to the robot.
type settings struct {
	Host       string `json:"host,omitempty"`
	SSHUser    string `json:"ssh_user,omitempty"`
	AutoDock   bool   `json:"auto_dock,omitempty"`
	AutoDockAt int    `json:"auto_dock_at,omitempty"` // battery percent
	LANAccess  bool   `json:"lan_access,omitempty"`   // web app on home Wi-Fi
	// ObstacleGuardOff disables slowing and stopping for obstacles ahead
	// during manual driving (moves, patrols and agents are always guarded).
	ObstacleGuardOff bool          `json:"obstacle_guard_off,omitempty"`
	Guard            GuardSettings `json:"guard"`
}

// GuardSettings configures guard mode: motion, people and pet alerts.
type GuardSettings struct {
	Armed       bool `json:"armed"`
	Sensitivity int  `json:"sensitivity"`        // 1 (least) to 5 (most)
	Notify      bool `json:"notify"`             // macOS notifications
	Objects     bool `json:"objects"`            // people and pets via the robot's detector
	Cooldown    int  `json:"cooldown,omitempty"` // seconds between alerts of one kind
}

func (g GuardSettings) normalized() GuardSettings {
	if g.Sensitivity < 1 || g.Sensitivity > 5 {
		g.Sensitivity = 3
	}
	if g.Cooldown < 5 || g.Cooldown > 600 {
		g.Cooldown = 30
	}
	return g
}

// dataDir returns a per-user directory next to the settings for app data
// such as events and routes, creating it if needed.
func dataDir(name string) (string, error) {
	path, err := settingsPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(path), name)
	return dir, os.MkdirAll(dir, 0o700)
}

// mediaDir returns ~/Movies/Scout (or the working directory outside the app
// bundle, like screenshots), for local video recordings.
func mediaDir() string {
	if !inAppBundle() {
		return screenshotDir()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(home, "Movies", "Scout")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "."
	}
	return dir
}

var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`)
var userPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

func validHost(host string) error {
	if len(host) > 253 || !hostPattern.MatchString(host) {
		return fmt.Errorf("enter a hostname or IPv4 address, e.g. 192.168.1.97")
	}
	return nil
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Scout", "settings.json"), nil
}

func loadSettings() settings {
	var s settings
	path, err := settingsPath()
	if err == nil {
		var data []byte
		if data, err = os.ReadFile(path); err == nil {
			err = json.Unmarshal(data, &s)
		}
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ignoring saved settings: %v\n", err)
		s = settings{}
	}
	if validHost(s.Host) != nil {
		s.Host = ""
	}
	if !userPattern.MatchString(s.SSHUser) {
		s.SSHUser = "root"
	}
	return s
}

var settingsSaveMu sync.Mutex

func (a *application) saveSettings() error {
	settingsSaveMu.Lock()
	defer settingsSaveMu.Unlock()
	a.settingsMu.Lock()
	s := a.settings
	a.settingsMu.Unlock()
	return saveSettingsLocked(s)
}

func saveSettings(s settings) error {
	settingsSaveMu.Lock()
	defer settingsSaveMu.Unlock()
	return saveSettingsLocked(s)
}

func saveSettingsLocked(s settings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "settings-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(data, '\n'))
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// target describes how to reach one robot.
type target struct {
	Host string // empty until the user enters one
	ROS  string // ROS master host:port
	SSH  string // user@host, or empty when SSH is disabled
}

// resolveTarget applies, in order: -host, an explicit -r, then the saved host.
// An explicit -ssh (including -ssh "") overrides the root@host default.
func resolveTarget(saved settings) (target, error) {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	t := target{Host: saved.Host}
	if set["r"] {
		host, _, err := net.SplitHostPort(*flagROSHostAddress)
		if err != nil {
			return t, fmt.Errorf("-r must be host:port: %w", err)
		}
		t.Host, t.ROS = host, *flagROSHostAddress
	}
	if *flagHost != "" {
		if err := validHost(*flagHost); err != nil {
			return t, fmt.Errorf("-host: %w", err)
		}
		t.Host = *flagHost
		if !set["r"] {
			t.ROS = ""
		}
	}
	return t.withHost(t.Host, saved.SSHUser, set["ssh"]), nil
}

// withHost fills in defaults for a host the user typed or that was saved.
func (t target) withHost(host, sshUser string, explicitSSH bool) target {
	t.Host = host
	if t.ROS == "" && host != "" {
		t.ROS = net.JoinHostPort(host, "11311")
	}
	switch {
	case *flagNoSSH:
		t.SSH = ""
	case explicitSSH:
		t.SSH = *flagSSH
	case host != "":
		t.SSH = sshUser + "@" + host
	}
	return t
}

func flagVisitSSH(set *bool) {
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "ssh" {
			*set = true
		}
	})
}

// apiToken returns the API token: SCOUT_API_TOKEN if set, otherwise a random
// token saved next to the settings (mode 0600) so paired browsers and
// scoutctl keep working across restarts. The second value says where it is.
func apiToken() (string, string, error) {
	if t := os.Getenv("SCOUT_API_TOKEN"); t != "" {
		return t, "$SCOUT_API_TOKEN", nil
	}
	path, err := settingsPath()
	if err != nil {
		return "", "", err
	}
	path = filepath.Join(filepath.Dir(path), "api-token")
	if data, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(data)); len(t) >= 32 {
			return t, path, nil
		}
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	t := hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", "", err
	}
	return t, path, nil
}

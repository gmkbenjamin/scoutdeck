package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/shell-company/scout/internal/agent"
)

// textField is a single-line keyboard input.
type textField struct {
	text  string
	max   int
	allow func(rune) bool
}

func (f *textField) update() {
	for _, r := range ebiten.AppendInputChars(nil) {
		if len(f.text)+len(string(r)) <= f.max && (f.allow == nil || f.allow(r)) {
			f.text += string(r)
		}
	}
	if d := inpututil.KeyPressDuration(ebiten.KeyBackspace); len(f.text) > 0 && (d == 1 || d > 30 && d%3 == 0) {
		r := []rune(f.text)
		f.text = string(r[:len(r)-1])
	}
}

func hostRune(r rune) bool {
	return r == '.' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func printable(r rune) bool { return r >= 0x20 && r != 0x7f }

func pressed(keys ...ebiten.Key) bool {
	for _, k := range keys {
		if inpututil.IsKeyJustPressed(k) {
			return true
		}
	}
	return false
}

type menuPage int

const (
	menuClosed menuPage = iota
	menuMain
	menuWiFiList
	menuWiFiCredentials
	menuWiFiConfirm
	menuReset
	menuBusy
	menuPhone
	menuRestore
	menuRoutes
	menuRouteName
	menuGuard
	menuCamera
	menuRecordings
	menuHealth
)

// Main menu entries, in display order.
const (
	miRoutes = iota
	miGuard
	miCamera
	miRecordings
	miHealth
	miObstacle
	miAutoDock
	miVoice
	miPhone
	miWiFi
	miReset
	miRestore
)

var menuItems = []struct {
	title, detail string
	ssh           bool // needs SSH
}{
	miRoutes:     {"Patrol routes", "Record a drive, replay it as a patrol, schedule it", false},
	miGuard:      {"Guard mode & alerts", "Motion, people and pet alerts with snapshots (G)", false},
	miCamera:     {"Camera", "Night vision, brightness, wide dynamic range, compression", false},
	miRecordings: {"Recordings on the robot", "Browse, download and delete the robot's own clips", false},
	miHealth:     {"Robot health", "Battery history, temperature, Wi-Fi signal, storage", false},
	miObstacle:   {"Obstacle guard", "", false}, // detail is drawn from the current setting
	miAutoDock:   {"Auto-dock on low battery", "", false},
	miVoice:      {"Voice assistant", "Say \"Scout, ...\" - Apple's on-device model, offline (V)", true},
	miPhone:      {"Phone & tablet", "Drive from iPhone, iPad or a browser over Tailscale", false},
	miWiFi:       {"Wi-Fi network", "Scan for networks and move the robot to one", true},
	miReset:      {"Reset settings", "Firmware reset: Wi-Fi, cloud link, maps, recordings. Keeps SSH.", true},
	miRestore:    {"Factory restore", "Reinstall the factory system from the robot's backup partition", true},
}

// robotMenu is the maintenance overlay (M). Background work reports through
// the mutex-protected fields; Update and Draw run on the UI goroutine.
type robotMenu struct {
	page   menuPage
	sel    int
	ssid   textField
	pass   textField
	field  int  // 0 SSID, 1 password
	custom bool // SSID typed rather than picked

	name       textField // new route name
	confirmDel string    // item awaiting a second Delete press
	feature    featureMenu

	mu       sync.Mutex
	phone    phoneAccess
	checking bool
	checks   []agent.RestoreCheck
	confirm  textField
	scanning bool
	networks []wifiInfo
	status   string
	bad      bool
}

func (m *robotMenu) set(status string, bad bool) {
	m.mu.Lock()
	m.status, m.bad = status, bad
	m.mu.Unlock()
}

func (m *robotMenu) snapshot() (bool, []wifiInfo, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scanning, m.networks, m.status, m.bad
}

func (m *robotMenu) open(g *Game) {
	*m = robotMenu{page: menuMain}
	g.app.control.Stop()
}

func (m *robotMenu) scan(a *application) {
	m.mu.Lock()
	if m.scanning {
		m.mu.Unlock()
		return
	}
	m.scanning, m.status, m.bad = true, "Scanning from the robot…", false
	m.mu.Unlock()
	go func() {
		nets, err := scanWiFi(a.connContext())
		m.mu.Lock()
		defer m.mu.Unlock()
		m.scanning = false
		if err != nil {
			m.status, m.bad = err.Error(), true
			return
		}
		m.networks, m.status, m.bad = nets, "", false
		if len(nets) == 0 {
			m.status = "No networks found - press R to scan again"
		}
	}()
}

// update returns true while the menu owns the keyboard.
func (m *robotMenu) update(g *Game) bool {
	if m.page == menuClosed {
		return false
	}
	a := g.app
	scanning, nets, _, _ := m.snapshot()
	switch m.page {
	case menuMain:
		switch {
		case pressed(ebiten.KeyEscape, ebiten.KeyM):
			m.page = menuClosed
		case pressed(ebiten.KeyArrowUp):
			m.sel = (m.sel + len(menuItems) - 1) % len(menuItems)
		case pressed(ebiten.KeyArrowDown):
			m.sel = (m.sel + 1) % len(menuItems)
		case m.sel == miAutoDock && pressed(ebiten.KeyArrowLeft, ebiten.KeyArrowRight):
			ad := a.autoDockSettings()
			if pressed(ebiten.KeyArrowLeft) {
				ad.Threshold = max(autoDockMin, ad.Threshold-5)
			} else {
				ad.Threshold = min(autoDockMax, ad.Threshold+5)
			}
			if err := a.setAutoDock(ad); err != nil {
				m.set(err.Error(), true)
			}
		case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
			if a.audio == nil && menuItems[m.sel].ssh {
				m.set("Needs SSH (the app was started with -no-ssh)", true)
				break
			}
			m.set("", false)
			switch m.sel {
			case miRoutes:
				m.page, m.sel = menuRoutes, 0
			case miGuard:
				m.page, m.sel = menuGuard, 0
			case miCamera:
				m.page, m.sel = menuCamera, 0
				m.loadCamera(a)
			case miRecordings:
				m.page, m.sel = menuRecordings, 0
				m.loadRecordings(a)
			case miHealth:
				m.page, m.sel = menuHealth, 0
				m.loadHealth(a)
			case miObstacle:
				if err := a.setManualObstacleGuard(!a.manualObstacleGuard()); err != nil {
					m.set(err.Error(), true)
				}
			case miWiFi:
				m.page, m.sel = menuWiFiList, 0
				m.scan(a)
			case miReset:
				m.page = menuReset
			case miPhone:
				m.page = menuPhone
				m.checkPhone(a)
				if code, _ := a.codes.Current(); code == "" {
					_, _ = a.codes.New()
				}
			case miVoice:
				m.page = menuClosed
				a.toggleVoice()
			case miAutoDock:
				ad := a.autoDockSettings()
				ad.Enabled = !ad.Enabled
				if err := a.setAutoDock(ad); err != nil {
					m.set(err.Error(), true)
				}
			case miRestore:
				m.page = menuRestore
				m.confirm = textField{max: 7, allow: func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }}
				m.checkRestore(a)
			}
		}
	case menuWiFiList:
		count := len(nets) + 1 // last entry: type a network name
		switch {
		case pressed(ebiten.KeyEscape):
			m.page, m.sel = menuMain, miWiFi
		case pressed(ebiten.KeyR) && !scanning:
			m.scan(a)
		case pressed(ebiten.KeyArrowUp):
			m.sel = (m.sel + count - 1) % count
		case pressed(ebiten.KeyArrowDown):
			m.sel = (m.sel + 1) % count
		case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
			m.custom = m.sel >= len(nets)
			m.ssid = textField{max: 32, allow: printable}
			m.pass = textField{max: 63, allow: printable}
			m.field = 1
			if m.custom {
				m.field = 0
			} else {
				m.ssid.text = nets[m.sel].SSID
			}
			m.page = menuWiFiCredentials
			m.set("", false)
		}
	case menuWiFiCredentials:
		switch {
		case pressed(ebiten.KeyEscape):
			m.page = menuWiFiList
		case pressed(ebiten.KeyTab) && m.custom:
			m.field = 1 - m.field
		case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
			if err := agent.ValidateWiFi(m.ssid.text, m.pass.text); err != nil {
				m.set(err.Error(), true)
				break
			}
			m.page = menuWiFiConfirm
			m.set("", false)
		default:
			if m.field == 0 {
				m.ssid.update()
			} else {
				m.pass.update()
			}
		}
	case menuWiFiConfirm:
		switch {
		case pressed(ebiten.KeyEscape):
			m.page = menuWiFiCredentials
		case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
			m.page = menuBusy
			m.applyWiFi(a, m.ssid.text, m.pass.text)
		}
	case menuReset:
		switch {
		case pressed(ebiten.KeyEscape):
			m.page = menuMain
		case pressed(ebiten.KeyY):
			m.page = menuBusy
			m.resetSettings(g)
		}
	case menuPhone:
		m.mu.Lock()
		pa, checking := m.phone, m.checking
		m.mu.Unlock()
		switch {
		case pressed(ebiten.KeyEscape):
			m.page, m.sel = menuMain, miPhone
		case a.apiToken == "":
			if pressed(ebiten.KeyEnter) {
				m.page = menuMain
			}
		case pressed(ebiten.KeyR) && !checking:
			m.checkPhone(a)
		case pressed(ebiten.KeyT) && !checking && pa.Tailscale && pa.URL == "":
			m.mu.Lock()
			m.checking = true
			m.mu.Unlock()
			m.set("Enabling Tailscale access…", false)
			go func() {
				err := enablePhoneAccess(a.ctx, *flagAPI)
				m.mu.Lock()
				m.checking = false
				m.mu.Unlock()
				if err != nil {
					m.set(err.Error(), true)
					return
				}
				m.set("", false)
				m.checkPhone(a)
			}()
		case pressed(ebiten.KeyN):
			if _, err := a.codes.New(); err != nil {
				m.set(err.Error(), true)
			} else {
				m.set("New one-time code; the previous one no longer works.", false)
			}
		case pressed(ebiten.KeyL):
			on := !lanOn(&Game{app: a})
			if err := a.setLANAccess(on); err != nil {
				m.set(err.Error(), true)
			} else if on {
				m.set("Home Wi-Fi access on. macOS may ask to allow incoming connections: choose Allow.", false)
			} else {
				m.set("Home Wi-Fi access off", false)
			}
		case pressed(ebiten.KeyB):
			code, err := a.codes.New()
			if err != nil {
				m.set(err.Error(), true)
				break
			}
			url := "http://" + *flagAPI + "/#pair=" + code
			if err := exec.Command("open", url).Start(); err != nil {
				m.set(err.Error(), true)
			}
		case pressed(ebiten.KeyC):
			if err := copyToClipboard(a.apiToken); err != nil {
				m.set(err.Error(), true)
			} else {
				m.set("Access code copied. Paste it on the device's pairing screen.", false)
			}
		}
	case menuRestore:
		m.mu.Lock()
		checks, checking := m.checks, m.checking
		m.mu.Unlock()
		switch {
		case pressed(ebiten.KeyEscape):
			m.page, m.sel = menuMain, miRestore
		case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
			if checking || !agent.RestoreReady(checks) {
				m.set("All checks must pass first", true)
			} else if m.confirm.text != "RESTORE" {
				m.set("Type RESTORE in capitals to confirm", true)
			} else {
				m.page = menuBusy
				m.factoryRestore(a)
			}
		default:
			m.confirm.update()
		}
	case menuBusy:
		if _, _, status, bad := m.snapshot(); bad && pressed(ebiten.KeyEscape) {
			m.page = menuMain
			_ = status
		}
	default:
		m.updateFeature(g)
	}
	return true
}

func (m *robotMenu) applyWiFi(a *application, ssid, key string) {
	m.set("Saving Wi-Fi settings on the robot…", false)
	go func() {
		ctx, cancel := context.WithTimeout(a.connContext(), 40*time.Second)
		defer cancel()
		if err := a.stopAll(ctx); err != nil {
			log.Printf("stop before Wi-Fi change: %v", err)
		}
		if err := a.audio.StageWiFi(ctx, ssid, key); err != nil {
			m.set(err.Error(), true)
			return
		}
		m.set("Switching networks…", false)
		if err := switchWiFi(ctx, ssid, key); err != nil {
			m.set(err.Error()+". The saved setting applies at next boot; the watchdog reverts it if it fails.", true)
			return
		}
		log.Printf("robot switching to Wi-Fi %q", ssid)
		a.lost("", fmt.Sprintf("The robot is joining %q. If it can't connect within %d minutes it restores its previous Wi-Fi and reboots. "+
			"Put this Mac on %q, then press Tab to find the robot (its address may change).", ssid, int(agent.WiFiRevertAfter/time.Minute), ssid))
	}()
}

func (m *robotMenu) resetSettings(g *Game) {
	a := g.app
	m.set("Resetting robot settings…", false)
	go func() {
		ctx, cancel := context.WithTimeout(a.connContext(), 30*time.Second)
		defer cancel()
		ssid, err := a.audio.HotspotSSID(ctx)
		if err != nil {
			m.set(err.Error(), true)
			return
		}
		if err := a.stopAll(ctx); err != nil {
			log.Printf("stop before reset: %v", err)
		}
		if err := a.audio.ResetSettings(ctx); err != nil {
			m.set(err.Error(), true)
			return
		}
		log.Printf("robot settings reset started; hotspot %s", ssid)
		a.suggestHost(hotspotAddress)
		a.lost("", fmt.Sprintf("Settings reset started. The robot reboots into hotspot mode in about a minute. "+
			"Join Wi-Fi %q (password r0123456) on this Mac, then press Enter to connect to %s.", ssid, hotspotAddress))
	}()
}

func (m *robotMenu) checkPhone(a *application) {
	m.mu.Lock()
	if m.checking {
		m.mu.Unlock()
		return
	}
	m.checking = true
	m.mu.Unlock()
	go func() {
		pa := checkPhoneAccess(a.ctx, *flagAPI)
		m.mu.Lock()
		m.phone, m.checking = pa, false
		m.mu.Unlock()
	}()
}

func (m *robotMenu) checkRestore(a *application) {
	m.mu.Lock()
	if m.checking {
		m.mu.Unlock()
		return
	}
	m.checking, m.checks = true, nil
	m.status, m.bad = "Checking the robot…", false
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(a.connContext(), 30*time.Second)
		defer cancel()
		checks, err := a.audio.RestorePreflight(ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.checks, m.checking = checks, false
		switch {
		case err != nil:
			m.status, m.bad = err.Error(), true
		case !agent.RestoreReady(checks):
			m.status, m.bad = "Not ready: fix the items marked NO, then reopen this page", true
		default:
			m.status, m.bad = "", false
		}
	}()
}

func (m *robotMenu) factoryRestore(a *application) {
	m.set("Starting factory restore…", false)
	go func() {
		ctx, cancel := context.WithTimeout(a.connContext(), 60*time.Second)
		defer cancel()
		ssid, err := a.audio.HotspotSSID(ctx)
		if err != nil {
			ssid = "robot-scout-xxxxxx"
		}
		if err := a.stopAll(ctx); err != nil {
			log.Printf("stop before restore: %v", err)
		}
		if err := a.audio.StartFactoryRestore(ctx); err != nil {
			m.set(err.Error(), true)
			return
		}
		log.Printf("factory restore started")
		a.suggestHost(hotspotAddress)
		a.lost("", fmt.Sprintf("Factory restore started. The robot reboots into recovery and reinstalls its system; this may take 10 minutes or more. "+
			"Keep it on the charger and don't switch it off. It comes back in hotspot mode: join Wi-Fi %q (password r0123456), then connect to %s. "+
			"Your SSH key was erased: run ssh-copy-id root@%s with the factory root password before using Wi-Fi setup or docking. "+
			"If SSH reports a changed host key, run ssh-keygen -R %s.", ssid, hotspotAddress, hotspotAddress, hotspotAddress))
	}()
}

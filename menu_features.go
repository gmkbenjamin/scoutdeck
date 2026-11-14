package main

import (
	"context"
	"fmt"
	"image/color"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
	"github.com/hajimehoshi/ebiten/v2/vector"

	"github.com/gmkbenjamin/scoutdeck/internal/motion"
)

// featureMenu holds data loaded in the background for the newer menu pages.
type featureMenu struct {
	mu       sync.Mutex
	loading  bool
	selReset bool
	camera   []CameraSetting
	recs     []RobotRecording
	health   map[string]any
}

func (f *featureMenu) busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loading
}

func (m *robotMenu) load(a *application, status string, fn func(ctx context.Context) error) {
	f := &m.feature
	f.mu.Lock()
	if f.loading {
		f.mu.Unlock()
		return
	}
	f.loading = true
	f.mu.Unlock()
	if status != "" {
		m.set(status, false)
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.connContext(), 2*time.Minute)
		defer cancel()
		err := fn(ctx)
		f.mu.Lock()
		f.loading = false
		f.mu.Unlock()
		if err != nil {
			m.set(err.Error(), true)
		} else if status != "" {
			m.set("", false)
		}
	}()
}

func (m *robotMenu) loadCamera(a *application) {
	m.load(a, "Reading camera settings…", func(ctx context.Context) error {
		list, err := a.CameraSettings(ctx)
		m.feature.mu.Lock()
		m.feature.camera = list
		m.feature.mu.Unlock()
		return err
	})
}

func (m *robotMenu) loadRecordings(a *application) {
	m.load(a, "Listing recordings on the robot…", func(ctx context.Context) error {
		list, err := a.RobotRecordings(ctx)
		m.feature.mu.Lock()
		m.feature.recs = list
		m.feature.mu.Unlock()
		if err == nil && len(list) == 0 {
			m.set("No recordings on the robot", false)
			return nil
		}
		return err
	})
}

func (m *robotMenu) loadHealth(a *application) {
	m.load(a, "Checking the robot…", func(ctx context.Context) error {
		h, err := a.Health(ctx)
		m.feature.mu.Lock()
		m.feature.health = h
		m.feature.mu.Unlock()
		return err
	})
}

var scheduleSteps = []int{0, 30, 60, 120, 240, 480, 1440}

func scheduleLabel(min int) string {
	switch {
	case min == 0:
		return "manual"
	case min < 60:
		return fmt.Sprintf("every %d min", min)
	case min == 60:
		return "hourly"
	case min == 1440:
		return "daily"
	}
	return fmt.Sprintf("every %d h", min/60)
}

// updateFeature handles keys on the newer menu pages.
func (m *robotMenu) updateFeature(g *Game) {
	a := g.app
	back := func(item int) { m.page, m.sel, m.confirmDel = menuMain, item, "" }
	switch m.page {
	case menuRoutes:
		routes := a.routes.List()
		recName, _, _ := a.routes.recordingRoute()
		head := 1 // "Record new route" or "Save route"
		if recName != "" {
			head = 2 // save, discard
		}
		count := head + len(routes)
		switch {
		case pressed(ebiten.KeyEscape):
			back(miRoutes)
		case pressed(ebiten.KeyArrowUp):
			m.sel, m.confirmDel = (m.sel+count-1)%count, ""
		case pressed(ebiten.KeyArrowDown):
			m.sel, m.confirmDel = (m.sel+1)%count, ""
		case m.sel < head && pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
			switch {
			case recName == "":
				m.page = menuRouteName
				m.name = textField{max: 40, allow: func(r rune) bool { return motion.ValidName(string(r)) == nil || r == ' ' }}
				m.set("", false)
			case m.sel == 0:
				r, err := a.FinishRecording(false)
				if err != nil {
					m.set(err.Error(), true)
				} else {
					m.set(fmt.Sprintf("Saved %s: %d steps, %.1f m, %s. Enter on it to patrol.", r.Name, len(r.Steps), r.Distance, fmtDuration(time.Duration(r.Duration*float64(time.Second)))), false)
				}
			default:
				_, _ = a.FinishRecording(true)
				m.set("Recording discarded", false)
				m.sel = 0
			}
		case m.sel >= head:
			r := routes[m.sel-head]
			switch {
			case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
				if err := a.Patrol(a.connContext(), r.Name); err != nil {
					m.set(err.Error(), true)
				} else {
					m.page = menuClosed
					a.notices.add("Patrol started: "+r.Name+" - X stops", noticeInfo)
				}
			case pressed(ebiten.KeyS):
				i := sort.SearchInts(scheduleSteps, r.EveryMinutes)
				next := scheduleSteps[(i+1)%len(scheduleSteps)]
				if _, err := a.routes.Update(r.Name, next, r.DockAfter); err != nil {
					m.set(err.Error(), true)
				} else if next > 0 {
					m.set(fmt.Sprintf("%s runs %s from the dock (charged to 50%%+, idle 2 min)", r.Name, scheduleLabel(next)), false)
				} else {
					m.set(r.Name+" runs only when you start it", false)
				}
			case pressed(ebiten.KeyD):
				if _, err := a.routes.Update(r.Name, r.EveryMinutes, !r.DockAfter); err != nil {
					m.set(err.Error(), true)
				}
			case pressed(ebiten.KeyDelete, ebiten.KeyBackspace):
				if m.confirmDel != r.Name {
					m.confirmDel = r.Name
					m.set("Press Delete again to delete "+r.Name, true)
				} else {
					m.confirmDel = ""
					if err := a.routes.Delete(r.Name); err != nil {
						m.set(err.Error(), true)
					} else {
						m.set("Deleted "+r.Name, false)
						m.sel = 0
					}
				}
			}
		}
	case menuRouteName:
		switch {
		case pressed(ebiten.KeyEscape):
			m.page = menuRoutes
		case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
			name := strings.TrimSpace(m.name.text)
			if err := a.StartRecording(name); err != nil {
				m.set(err.Error(), true)
				break
			}
			m.page = menuClosed
			a.notices.add("Recording route "+name+": drive it now, then M > Patrol routes > Save", noticeGood)
		default:
			m.name.update()
		}
	case menuGuard:
		gs := a.guardSettings()
		const rows = 6
		changed := false
		left, right := pressed(ebiten.KeyArrowLeft), pressed(ebiten.KeyArrowRight)
		enter := pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter)
		switch {
		case pressed(ebiten.KeyEscape):
			back(miGuard)
		case pressed(ebiten.KeyArrowUp):
			m.sel = (m.sel + rows - 1) % rows
		case pressed(ebiten.KeyArrowDown):
			m.sel = (m.sel + 1) % rows
		case m.sel == 0 && (enter || left || right):
			gs.Armed, changed = !gs.Armed, true
		case m.sel == 1 && (left || right):
			d := 1
			if left {
				d = -1
			}
			gs.Sensitivity, changed = max(1, min(5, gs.Sensitivity+d)), true
		case m.sel == 2 && (enter || left || right):
			gs.Objects, changed = !gs.Objects, true
		case m.sel == 3 && (enter || left || right):
			gs.Notify, changed = !gs.Notify, true
		case m.sel == 4 && (left || right):
			steps := []int{10, 30, 60, 120, 300}
			i := sort.SearchInts(steps, gs.Cooldown)
			if left {
				i = max(0, i-1)
			} else {
				i = min(len(steps)-1, i+1)
			}
			gs.Cooldown, changed = steps[i], true
		case m.sel == 5 && enter:
			if m.confirmDel != "events" {
				m.confirmDel = "events"
				m.set("Press Enter again to delete every event and snapshot", true)
			} else {
				m.confirmDel = ""
				if err := a.events.Clear(); err != nil {
					m.set(err.Error(), true)
				} else {
					m.set("Events cleared", false)
				}
			}
		case pressed(ebiten.KeyO):
			if a.events.Dir != "" {
				_ = exec.Command("open", a.events.Dir).Start()
			}
		}
		if changed {
			if err := a.setGuard(gs); err != nil {
				m.set(err.Error(), true)
			} else {
				m.set("", false)
			}
		}
	case menuCamera:
		m.feature.mu.Lock()
		list := append([]CameraSetting(nil), m.feature.camera...)
		m.feature.mu.Unlock()
		switch {
		case pressed(ebiten.KeyEscape):
			back(miCamera)
		case len(list) == 0:
			if pressed(ebiten.KeyR) {
				m.loadCamera(a)
			}
		case pressed(ebiten.KeyArrowUp):
			m.sel = (m.sel + len(list) - 1) % len(list)
		case pressed(ebiten.KeyArrowDown):
			m.sel = (m.sel + 1) % len(list)
		case pressed(ebiten.KeyArrowLeft, ebiten.KeyArrowRight, ebiten.KeyEnter):
			p := list[min(m.sel, len(list)-1)]
			step := int32(1)
			if p.Max-p.Min > 10 {
				step = 10
			}
			v := p.Value
			switch {
			case pressed(ebiten.KeyArrowLeft):
				v = max(p.Min, v-step)
			case pressed(ebiten.KeyArrowRight):
				v = min(p.Max, v+step)
			default: // Enter cycles
				v++
				if v > p.Max {
					v = p.Min
				}
			}
			if v == p.Value || m.feature.busy() {
				break
			}
			m.load(a, "", func(ctx context.Context) error {
				list, err := a.SetCamera(ctx, p.Name, v)
				if err == nil {
					m.feature.mu.Lock()
					m.feature.camera = list
					m.feature.mu.Unlock()
				}
				return err
			})
		}
	case menuRecordings:
		m.feature.mu.Lock()
		list := append([]RobotRecording(nil), m.feature.recs...)
		if m.feature.selReset {
			m.sel = 0
			m.feature.selReset = false
		}
		m.feature.mu.Unlock()
		switch {
		case pressed(ebiten.KeyEscape):
			back(miRecordings)
		case pressed(ebiten.KeyR):
			m.loadRecordings(a)
		case pressed(ebiten.KeyO):
			_ = exec.Command("open", mediaDir()).Start()
		case len(list) == 0:
		case pressed(ebiten.KeyArrowUp):
			m.sel, m.confirmDel = (m.sel+len(list)-1)%len(list), ""
		case pressed(ebiten.KeyArrowDown):
			m.sel, m.confirmDel = (m.sel+1)%len(list), ""
		case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
			r := list[min(m.sel, len(list)-1)]
			m.load(a, "Downloading "+r.Name+"…", func(ctx context.Context) error {
				path, err := a.DownloadRobotRecording(ctx, r.ID, mediaDir())
				if err != nil {
					return err
				}
				_ = exec.Command("open", path).Start()
				m.set("Saved to "+path, false)
				return nil
			})
		case pressed(ebiten.KeyDelete, ebiten.KeyBackspace):
			r := list[min(m.sel, len(list)-1)]
			if m.confirmDel != r.ID {
				m.confirmDel = r.ID
				m.set("Press Delete again to delete "+r.Name+" from the robot", true)
				break
			}
			m.confirmDel = ""
			m.load(a, "Deleting…", func(ctx context.Context) error {
				if err := a.DeleteRobotRecording(ctx, r.ID); err != nil {
					return err
				}
				list, err := a.RobotRecordings(ctx)
				m.feature.mu.Lock()
				m.feature.recs = list
				m.feature.selReset = true
				m.feature.mu.Unlock()
				return err
			})
		}
	case menuHealth:
		switch {
		case pressed(ebiten.KeyEscape):
			back(miHealth)
		case pressed(ebiten.KeyR):
			m.loadHealth(a)
		}
	}
}

// featureMenuSize returns the panel size for the newer pages.
func featureMenuSize(page menuPage) (float64, float64, bool) {
	switch page {
	case menuRoutes:
		return 1000, 760, true
	case menuRouteName:
		return 820, 420, true
	case menuGuard:
		return 900, 700, true
	case menuCamera:
		return 860, 640, true
	case menuRecordings:
		return 1000, 760, true
	case menuHealth:
		return 1080, 800, true
	}
	return 0, 0, false
}

// drawFeature draws a newer page; it returns the title and footer.
func (h *hud) drawFeature(screen *ebiten.Image, g *Game, x, y, pw, ph float64) (string, string) {
	m := &g.menu
	a := g.app
	cy := y + 110
	row := func(label, detail, value string, selected bool, valueCol color.Color) {
		if selected {
			h.panel(screen, x+24, cy-24, pw-48, 58, color.NRGBA{79, 209, 255, 45})
		}
		h.text(screen, label, 20, true, x+44, cy-4, colText, text.AlignStart)
		if detail != "" {
			h.text(screen, h.fit(detail, 15, false, pw-340), 15, false, x+44, cy+18, colDim, text.AlignStart)
		}
		if value != "" {
			h.mono(screen, value, 18, x+pw-44, cy+4, valueCol, text.AlignEnd)
		}
		cy += 64
	}
	switch m.page {
	case menuRoutes:
		routes := a.routes.List()
		recName, steps, recFor := a.routes.recordingRoute()
		i := 0
		if recName == "" {
			row("Record a new route…", "Name it, then drive the patrol by hand from the dock", "", m.sel == 0, colText)
			i = 1
		} else {
			row("Save route "+recName, fmt.Sprintf("%d steps recorded in %s", steps, fmtDuration(recFor)), "● REC", m.sel == 0, colBad)
			row("Discard recording", "", "", m.sel == 1, colText)
			i = 2
		}
		start := max(0, m.sel-i-5)
		for j, r := range routes {
			if j < start || j >= start+7 {
				continue
			}
			dock := ""
			if r.DockAfter {
				dock = " · docks after"
			}
			last := "never run"
			if !r.LastRun.IsZero() {
				last = "last run " + ago(time.Since(r.LastRun))
			}
			row(r.Name, fmt.Sprintf("%d steps · %.1f m · %s%s · %s", len(r.Steps), r.Distance, fmtDuration(time.Duration(r.Duration*float64(time.Second))), dock, last),
				strings.ToUpper(scheduleLabel(r.EveryMinutes)), m.sel == i+j, colAccent)
		}
		if len(routes) == 0 {
			h.text(screen, "No routes yet. Start at the dock so patrols begin and end there.", 17, false, x+44, cy, colDim, text.AlignStart)
		}
		return "Patrol routes", "Enter run / record      S schedule      D dock after      Delete remove      Esc back"
	case menuRouteName:
		h.text(screen, "Put the robot where the patrol should start (ideally on the dock), then name the route.", 17, false, x+40, cy-10, colDim, text.AlignStart)
		cy += 30
		h.text(screen, "ROUTE NAME", 14, true, x+40, cy, colDim, text.AlignStart)
		cy += 18
		h.panel(screen, x+40, cy, pw-80, 56, color.NRGBA{0, 0, 0, 120})
		vector.StrokeRect(screen, float32((x+40)*h.s), float32(cy*h.s), float32((pw-80)*h.s), float32(56*h.s), float32(2*h.s), colAccent, true)
		shown := m.name.text
		if time.Now().UnixMilli()/530%2 == 0 {
			shown += "|"
		}
		h.text(screen, shown, 24, false, x+60, cy+28, colText, text.AlignStart)
		cy += 90
		h.text(screen, "Turns and straight runs replay with gyro/odometry feedback; curves replay by time.", 15, false, x+40, cy, colDim, text.AlignStart)
		return "New patrol route", "Enter start recording      Esc back"
	case menuGuard:
		gs := a.guardSettings()
		counts := a.events.Counts(time.Now().Truncate(24 * time.Hour))
		onOffCol := func(on bool) (string, color.Color) {
			if on {
				return "ON", colGood
			}
			return "OFF", colDim
		}
		v, c := onOffCol(gs.Armed)
		if gs.Armed {
			c = colBad
		}
		row("Guard mode", "Watch the camera while the robot is parked (G toggles)", v, m.sel == 0, c)
		row("Motion sensitivity", "1 ignores small changes · 5 catches small movements", strings.Repeat("■", gs.Sensitivity)+strings.Repeat("□", 5-gs.Sensitivity), m.sel == 1, colAccent)
		v, c = onOffCol(gs.Objects)
		row("People and pets", "The robot's on-board model reports people, cats and dogs", v, m.sel == 2, c)
		v, c = onOffCol(gs.Notify)
		row("Mac notifications", "Notification Center alert with each event", v, m.sel == 3, c)
		row("Alert cooldown", "Minimum time between alerts of the same kind", fmt.Sprintf("%ds", gs.Cooldown), m.sel == 4, colAccent)
		row("Clear events", fmt.Sprintf("Today: %d motion · %d people · %d pets · %d other", counts["motion"], counts["person"], counts["cat"]+counts["dog"],
			counts["patrol"]+counts["dock"]+counts["info"]), "", m.sel == 5, colText)
		return "Guard mode & alerts", "Enter / Left / Right change      O open snapshots folder      Esc back"
	case menuCamera:
		m.feature.mu.Lock()
		list := append([]CameraSetting(nil), m.feature.camera...)
		m.feature.mu.Unlock()
		for i, p := range list {
			val := fmt.Sprint(p.Value)
			if len(p.Choices) > int(p.Value-p.Min) && p.Value >= p.Min {
				val = p.Choices[p.Value-p.Min]
			}
			// A small bar for numeric ranges.
			if p.Choices == nil {
				bx, bw := x+pw-330, 160.0
				h.fill(screen, bx, cy-2, bw, 6, withAlpha(colText, 30))
				h.fill(screen, bx, cy-2, bw*float64(p.Value-p.Min)/float64(max(1, p.Max-p.Min)), 6, colAccent)
			}
			row(p.Label, fmt.Sprintf("%s · %d..%d", p.Name, p.Min, p.Max), val, m.sel == i, colAccent)
		}
		return "Camera", "Left / Right change      Enter cycle      R reload      Esc back"
	case menuRecordings:
		m.feature.mu.Lock()
		list := append([]RobotRecording(nil), m.feature.recs...)
		if m.feature.selReset {
			m.sel = 0
			m.feature.selReset = false
		}
		m.feature.mu.Unlock()
		start := max(0, m.sel-6)
		for i, r := range list {
			if i < start || i >= start+8 {
				continue
			}
			dur := ""
			if r.Seconds > 0 {
				dur = fmtDuration(time.Duration(r.Seconds)*time.Second) + " · "
			}
			row(r.Created.Local().Format("Mon 2 Jan 15:04"), fmt.Sprintf("%s · %s%.1f MB · %s", r.Kind, dur, float64(r.Bytes)/1e6, r.Name), "", m.sel == i, colText)
		}
		return "Recordings on the robot", "Enter download & open      Delete remove      R refresh      O open folder      Esc back"
	case menuHealth:
		m.feature.mu.Lock()
		hl := m.feature.health
		m.feature.mu.Unlock()
		h.drawHealth(screen, g, hl, x, y, pw)
		return "Robot health", "R refresh      Esc back"
	}
	return "", ""
}

func (h *hud) drawHealth(screen *ebiten.Image, g *Game, hl map[string]any, x, y, pw float64) {
	num := func(k string) (float64, bool) {
		v, ok := hl[k].(float64)
		return v, ok
	}
	str := func(k string) string {
		v, _ := hl[k].(string)
		return v
	}
	type tile struct {
		label, value string
		c            color.Color
	}
	var tiles []tile
	if v, ok := num("temperature_c"); ok {
		c := colGood
		if v > 70 {
			c = colWarn
		}
		if v > 80 {
			c = colBad
		}
		tiles = append(tiles, tile{"CPU TEMP", fmt.Sprintf("%.0f°C", v), c})
	}
	if v, ok := num("wifi_signal"); ok {
		c := colGood
		if v < -67 {
			c = colWarn
		}
		if v < -78 {
			c = colBad
		}
		tiles = append(tiles, tile{"WI-FI " + strings.ToUpper(h.fit(str("wifi_ssid"), 12, true, 120)), fmt.Sprintf("%.0f dBm", v), c})
	}
	if v, ok := num("battery_volts"); ok {
		tiles = append(tiles, tile{"BATTERY", fmt.Sprintf("%.2f V", v), colAccent})
	}
	if v, ok := num("uptime"); ok {
		d := time.Duration(v) * time.Second
		tiles = append(tiles, tile{"UPTIME", fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24), colAccent})
	}
	if u, ok := num("storage_used_kb"); ok {
		if t, ok := num("storage_total_kb"); ok && t > 0 {
			tiles = append(tiles, tile{"STORAGE", fmt.Sprintf("%.0f%%", u/t*100), colAccent})
		}
	}
	if u, ok := num("memory_used_kb"); ok {
		if t, ok := num("memory_total_kb"); ok && t > 0 {
			tiles = append(tiles, tile{"MEMORY", fmt.Sprintf("%.0f%%", u/t*100), colAccent})
		}
	}
	tw := (pw - 80 - 5*12) / 6
	for i, t := range tiles {
		if i >= 6 {
			break
		}
		tx := x + 40 + float64(i)*(tw+12)
		h.panel(screen, tx, y+90, tw, 96, color.NRGBA{255, 255, 255, 10})
		h.text(screen, h.fit(t.label, 12, true, tw-20), 12, true, tx+12, y+112, colDim, text.AlignStart)
		h.mono(screen, t.value, 24, tx+12, y+152, t.c, text.AlignStart)
	}
	cy := y + 220
	h.text(screen, "BATTERY · LAST 24 HOURS", 13, true, x+40, cy, colDim, text.AlignStart)
	// Battery chart.
	gx, gy, gw, gh := x+40, cy+16, pw-80, 260.0
	h.panel(screen, gx, gy, gw, gh, color.NRGBA{0, 0, 0, 90})
	for _, p := range []float64{0.25, 0.5, 0.75} {
		h.line(screen, gx, gy+gh*(1-p), gx+gw, gy+gh*(1-p), 1, withAlpha(colText, 20))
		h.mono(screen, fmt.Sprintf("%.0f%%", p*100), 12, gx+6, gy+gh*(1-p)-10, colDim, text.AlignStart)
	}
	samples := g.app.battery.since(time.Now().Add(-24 * time.Hour))
	if len(samples) < 2 {
		h.text(screen, "Collecting… one sample a minute while the app is connected", 16, false, gx+gw/2, gy+gh/2, colDim, text.AlignCenter)
	} else {
		t0 := time.Now().Add(-24 * time.Hour)
		px := func(t time.Time) float64 { return gx + gw*t.Sub(t0).Hours()/24 }
		py := func(p int) float64 { return gy + gh*(1-float64(p)/100) }
		for i := 1; i < len(samples); i++ {
			a, b := samples[i-1], samples[i]
			if b.At.Sub(a.At) > 10*time.Minute {
				continue // gap: the app was not connected
			}
			c := colGood
			if b.Charging {
				c = colAccent
			} else if b.Percent < 25 {
				c = colWarn
			}
			h.line(screen, px(a.At), py(a.Percent), px(b.At), py(b.Percent), 3, c)
		}
	}
	for i := 0; i <= 4; i++ {
		t := time.Now().Add(-time.Duration(24-i*6) * time.Hour)
		h.mono(screen, t.Format("15:04"), 12, gx+gw*float64(i)/4, gy+gh+16, colDim, text.AlignCenter)
	}
	cy = gy + gh + 50
	lines := []string{}
	if fw := str("firmware"); fw != "" {
		lines = append(lines, "Firmware "+fw)
	}
	if ip := str("wifi_ip"); ip != "" {
		lines = append(lines, "Address "+ip)
	}
	if v, ok := num("wifi_freq"); ok {
		lines = append(lines, fmt.Sprintf("%.1f GHz", v/1000))
	}
	if l := str("load"); l != "" {
		lines = append(lines, "Load "+l)
	}
	if g.app.sensors.tracker.OdometryValid() {
		lines = append(lines, "Wheel odometry: working")
	} else {
		lines = append(lines, "Wheel odometry: not seen yet (drive a little)")
	}
	h.text(screen, strings.Join(lines, "   ·   "), 15, false, x+40, cy, colDim, text.AlignStart)
	if e := str("error"); e != "" {
		h.text(screen, h.fit(e, 15, false, pw-80), 15, false, x+40, cy+26, colBad, text.AlignStart)
	}
}

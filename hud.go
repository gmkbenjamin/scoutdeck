package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
	"github.com/hajimehoshi/ebiten/v2/vector"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"
	"rsc.io/qr"

	"github.com/shell-company/scout/internal/agent"
	"github.com/shell-company/scout/internal/docking"
	"github.com/shell-company/scout/internal/motion"
)

var (
	colText   = color.NRGBA{236, 239, 244, 255}
	colDim    = color.NRGBA{150, 158, 170, 255}
	colFaint  = color.NRGBA{255, 255, 255, 36}
	colPanel  = color.NRGBA{6, 12, 20, 200}
	colAccent = color.NRGBA{79, 209, 255, 255}
	colGood   = color.NRGBA{61, 220, 132, 255}
	colWarn   = color.NRGBA{255, 176, 32, 255}
	colBad    = color.NRGBA{255, 77, 77, 255}
	colAgent  = color.NRGBA{180, 140, 255, 255}
	colBack   = color.NRGBA{16, 18, 22, 255}
)

// Notices are short messages shown under the top bar for a few seconds.
type noticeKind int

const (
	noticeInfo noticeKind = iota
	noticeGood
	noticeBad
)

const noticeLife = 5 * time.Second

type notice struct {
	text string
	kind noticeKind
	at   time.Time
}

type notices struct {
	mu   sync.Mutex
	list []notice
}

func (n *notices) add(text string, kind noticeKind) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, notice{text, kind, time.Now()})
	if len(n.list) > 4 {
		n.list = n.list[len(n.list)-4:]
	}
}

func (n *notices) current(now time.Time) []notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []notice
	for _, v := range n.list {
		if now.Sub(v.at) < noticeLife {
			out = append(out, v)
		}
	}
	n.list = out
	return append([]notice(nil), out...)
}

type hud struct {
	qrText        string
	qrImage       *ebiten.Image
	regular, bold *text.GoTextFaceSource
	monoSrc       *text.GoTextFaceSource
	over          overlay
	thumbs        thumbCache
	video         image.Rectangle // where the camera image is drawn, in pixels
	faces         map[[2]int]*text.GoTextFace
	frameAt       time.Time
	frames        int
	fpsAt         time.Time
	fps           int
	s             float64 // pixels per 1080p unit
	noticeY       float64 // where notices start below the banners
	rangeAt       time.Time
	rangeM        float64
	rangeOK       bool
}

func newHUD() *hud {
	h := &hud{faces: map[[2]int]*text.GoTextFace{}}
	var err error
	if h.regular, err = text.NewGoTextFaceSource(bytes.NewReader(goregular.TTF)); err != nil {
		log.Fatal(err)
	}
	if h.bold, err = text.NewGoTextFaceSource(bytes.NewReader(gobold.TTF)); err != nil {
		log.Fatal(err)
	}
	if h.monoSrc, err = text.NewGoTextFaceSource(bytes.NewReader(gomonobold.TTF)); err != nil {
		log.Fatal(err)
	}
	return h
}

func (h *hud) face(size float64, bold bool) *text.GoTextFace {
	px := int(math.Round(size * h.s))
	key := [2]int{px, 0}
	src := h.regular
	if bold {
		key[1], src = 1, h.bold
	}
	f, ok := h.faces[key]
	if !ok {
		f = &text.GoTextFace{Source: src, Size: float64(px)}
		h.faces[key] = f
	}
	return f
}

// Layout renders at the display's native resolution so text stays sharp.
func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	scale := 1.0
	if mon := ebiten.Monitor(); mon != nil {
		scale = mon.DeviceScaleFactor()
	}
	return int(float64(outsideWidth) * scale), int(float64(outsideHeight) * scale)
}

func (g *Game) Draw(screen *ebiten.Image) { g.hud.draw(screen, g) }

// Drawing helpers take 1080p-relative units.

func (h *hud) fill(dst *ebiten.Image, x, y, w, ht float64, c color.Color) {
	vector.FillRect(dst, float32(x*h.s), float32(y*h.s), float32(w*h.s), float32(ht*h.s), c, false)
}

func (h *hud) panel(dst *ebiten.Image, x, y, w, ht float64, bg color.Color) {
	r := float32(10 * h.s)
	X, Y, W, H := float32(x*h.s), float32(y*h.s), float32(w*h.s), float32(ht*h.s)
	var p vector.Path
	p.MoveTo(X+r, Y)
	p.ArcTo(X+W, Y, X+W, Y+H, r)
	p.ArcTo(X+W, Y+H, X, Y+H, r)
	p.ArcTo(X, Y+H, X, Y, r)
	p.ArcTo(X, Y, X+W, Y, r)
	p.Close()
	op := &vector.DrawPathOptions{AntiAlias: true}
	op.ColorScale.ScaleWithColor(bg)
	vector.FillPath(dst, &p, nil, op)
	op = &vector.DrawPathOptions{AntiAlias: true}
	op.ColorScale.ScaleWithColor(colFaint)
	vector.StrokePath(dst, &p, &vector.StrokeOptions{Width: float32(1.5 * h.s)}, op)
}

func (h *hud) text(dst *ebiten.Image, s string, size float64, bold bool, x, y float64, c color.Color, align text.Align) {
	op := &text.DrawOptions{}
	op.GeoM.Translate(x*h.s, y*h.s)
	op.ColorScale.ScaleWithColor(c)
	op.PrimaryAlign = align
	op.SecondaryAlign = text.AlignCenter
	text.Draw(dst, s, h.face(size, bold), op)
}

func (h *hud) width(s string, size float64, bold bool) float64 {
	w, _ := text.Measure(s, h.face(size, bold), 0)
	return w / h.s
}

// fit shortens s with an ellipsis so it fits in max units.
func (h *hud) fit(s string, size float64, bold bool, max float64) string {
	if h.width(s, size, bold) <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && h.width(string(r)+"…", size, bold) > max {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func (h *hud) wrap(s string, size float64, max float64, lines int) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		next := strings.TrimSpace(line + " " + word)
		if line != "" && h.width(next, size, false) > max {
			out = append(out, line)
			line = word
			continue
		}
		line = next
	}
	if line != "" {
		out = append(out, line)
	}
	if len(out) > lines {
		out = out[:lines]
		out[lines-1] = h.fit(out[lines-1]+" …", size, false, max)
	}
	return out
}

func (h *hud) pullFrame() {
	select {
	case data := <-cameraData:
		img, _, err := ebitenutil.NewImageFromReader(bytes.NewReader(data))
		if err != nil {
			if *flagVerbose {
				log.Printf("camera frame: %v", err)
			}
			return
		}
		if lastCameraFeed != nil {
			lastCameraFeed.Deallocate()
		}
		lastCameraFeed = img
		h.frameAt = time.Now()
		h.frames++
	default:
	}
	if now := time.Now(); now.Sub(h.fpsAt) >= time.Second {
		h.fps, h.frames, h.fpsAt = h.frames, 0, now
	}
}

func (h *hud) draw(screen *ebiten.Image, g *Game) {
	W, H := float64(screen.Bounds().Dx()), float64(screen.Bounds().Dy())
	h.s = H / 1080
	w := W / h.s // width in 1080p units
	now := time.Now()
	h.pullFrame()
	screen.Fill(colBack)

	ready := g.inputTest || g.app.ready.Load()
	if !ready {
		h.drawConnect(screen, g, w)
		h.drawNotices(screen, g, w, now)
		return
	}
	videoLive := lastCameraFeed != nil && now.Sub(h.frameAt) < 3*time.Second
	h.video = image.Rectangle{}
	if lastCameraFeed != nil {
		iw, ih := float64(lastCameraFeed.Bounds().Dx()), float64(lastCameraFeed.Bounds().Dy())
		sc := min(W/iw, H/ih)
		op := &ebiten.DrawImageOptions{Filter: ebiten.FilterLinear}
		op.GeoM.Scale(sc, sc)
		ox, oy := (W-iw*sc)/2, (H-ih*sc)/2
		op.GeoM.Translate(ox, oy)
		if !videoLive {
			op.ColorScale.Scale(0.35, 0.35, 0.35, 1)
		}
		screen.DrawImage(lastCameraFeed, op)
		h.video = image.Rect(int(ox), int(oy), int(ox+iw*sc), int(oy+ih*sc))
	}
	full := !g.cleanHUD
	if full {
		h.drawOverlay(screen, w)
	}
	if !videoLive && !g.inputTest {
		msg := "Waiting for video…"
		if lastCameraFeed != nil {
			msg = "Video paused - last frame shown"
		}
		h.text(screen, msg, 26, false, w/2, 470, colDim, text.AlignCenter)
	}
	if !g.inputTest {
		h.drawDetections(screen, g, now)
	}
	h.drawReticle(screen, g, w, now)
	h.drawTopBar(screen, g, w, videoLive)
	if full && !g.inputTest {
		bottom := h.drawMiniMap(screen, g, w, now)
		h.drawEventFeed(screen, g, w, bottom, now)
	}
	h.drawDrive(screen, g)
	bottomY := 1080 - 24.0
	if !g.inputTest {
		op := g.app.dock.Snapshot()
		if h.dockVisible(op, now) {
			h.drawDock(screen, op, w, now)
			bottomY -= 150 + 12
		}
		h.drawActivity(screen, g, w, now, bottomY-84)
	}
	bannerY := 84.0
	if g.inputStatus.Stop {
		msg := "STOPPED - release all controls to resume"
		tw := h.width(msg, 24, true) + 60
		h.panel(screen, (w-tw)/2, bannerY, tw, 54, color.NRGBA{150, 20, 20, 210})
		h.text(screen, msg, 24, true, w/2, bannerY+27, colText, text.AlignCenter)
		bannerY += 66
	}
	if g.inputTest {
		msg := "INPUT TEST - no robot connected, nothing is sent"
		tw := h.width(msg, 22, true) + 60
		h.panel(screen, (w-tw)/2, bannerY, tw, 48, color.NRGBA{120, 80, 0, 210})
		h.text(screen, msg, 22, true, w/2, bannerY+24, colText, text.AlignCenter)
		bannerY += 60
	}
	h.noticeY = bannerY
	h.drawNotices(screen, g, w, now)
	h.drawHelp(screen, g, w)
	if !g.inputTest && g.menu.page == menuClosed && !g.showHelp {
		h.drawPhonePanel(screen, g, w)
	}
	h.drawMenu(screen, g, w)
}

func (h *hud) drawTopBar(screen *ebiten.Image, g *Game, w float64, videoLive bool) {
	// Glass bar with a thin accent line.
	for i := 0; i < 8; i++ {
		h.fill(screen, 0, float64(i)*8, w, 8, color.NRGBA{2, 6, 12, uint8(200 - i*12)})
	}
	h.fill(screen, 0, 64, w, 1.5, withAlpha(colAccent, 70))
	_, _, t := g.app.connState()
	dot, status := colGood, fmt.Sprintf("LIVE %2d FPS", h.fps)
	switch {
	case g.inputTest:
		dot, status = colWarn, "INPUT TEST"
	case !videoLive:
		dot, status = colWarn, "NO VIDEO"
	}
	h.dot(screen, 30, 32, 6, dot)
	h.glow(screen, "SCOUT", 25, 48, 32, colText, text.AlignStart)
	x := 48 + h.width("SCOUT", 25, true) + 18
	if t.Host != "" && !g.inputTest {
		h.mono(screen, t.Host, 17, x, 32, colDim, text.AlignStart)
		x += h.monoWidth(t.Host, 17) + 20
	}
	h.mono(screen, status, 16, x, 32, dot, text.AlignStart)
	x += h.monoWidth(status, 16) + 20
	if !g.inputTest {
		sshCol, sshText := colDim, "SSH OFF"
		if t.SSH != "" {
			sshCol, sshText = colGood, "SSH"
		}
		x = h.chip(screen, x, sshText, sshCol)
		if rs := g.app.rec.status(); rs.Active {
			lw := h.width("REC 00:00", 15, true) + 40
			h.panel(screen, x, 19, lw, 28, withAlpha(colBad, 50))
			h.dot(screen, x+16, 33, 6, colBad)
			h.mono(screen, "REC "+fmtDuration(time.Duration(rs.Seconds*float64(time.Second))), 15, x+28, 33, colBad, text.AlignStart)
			x += lw + 10
		}
		if g.app.guardSettings().Armed {
			x = h.chip(screen, x, "GUARD ARMED", colBad)
		}
		g.app.sensors.mu.Lock()
		night := g.app.sensors.night
		g.app.sensors.mu.Unlock()
		if night == 1 {
			x = h.chip(screen, x, "NIGHT IR", colAgent)
		}
		if !g.app.manualObstacleGuard() {
			x = h.chip(screen, x, "GUARD RAIL OFF", colWarn)
		}
		if secs := g.app.autoDockCountdown(); secs > 0 {
			x = h.chip(screen, x, fmt.Sprintf("LOW BATTERY · DOCKING IN %ds", secs), colBad)
		}
		if vs := g.app.voice.State(); vs != "" {
			vc := map[string]color.NRGBA{"listening": colGood, "awake": colAccent, "thinking": colAgent, "speaking": colWarn}[vs]
			if vc.A == 0 {
				vc = colDim
			}
			h.chip(screen, x, "VOICE "+strings.ToUpper(vs), vc)
		}
	}

	// Segmented battery on the right.
	batteryMu.RLock()
	pct, state, at := batteryPercent, batteryChargeState, batteryAt
	batteryMu.RUnlock()
	bw, bh := 74.0, 26.0
	bx, by := w-28-bw, 19.0
	h.panel(screen, bx, by, bw, bh, color.NRGBA{0, 0, 0, 60})
	h.fill(screen, bx+bw+1, by+8, 5, 10, colDim)
	label, pctText, fillCol, labelCol := "BATTERY", "--", colDim, colDim
	if pct >= 0 && !g.inputTest {
		pctText = fmt.Sprintf("%d%%", pct)
		fillCol = colGood
		if pct < 40 {
			fillCol = colWarn
		}
		if pct < 15 {
			fillCol = colBad
		}
		cells := int(math.Ceil(float64(pct) / 20))
		cw := (bw - 8 - 4*3) / 5
		for i := 0; i < 5; i++ {
			c := withAlpha(colText, 22)
			if i < cells {
				c = fillCol
			}
			h.fill(screen, bx+4+float64(i)*(cw+3), by+4, cw, bh-8, c)
		}
		labelCol = colDim
		switch state {
		case 0:
			label, labelCol = "CHARGING", colAccent
		case 1:
			label = "ON BATTERY"
		case 2:
			label, labelCol = "ON DOCK · FULL", colGood
		}
		if time.Since(at) > 30*time.Second {
			label += " (STALE)"
		}
	}
	h.mono(screen, pctText, 22, bx-12, 32, colText, text.AlignEnd)
	h.text(screen, label, 15, true, bx-24-h.monoWidth(pctText, 22), 32, labelCol, text.AlignEnd)
}

func (h *hud) drawDrive(screen *ebiten.Image, g *Game) {
	state := g.app.control.Snapshot()
	owner := state.Owner
	if !g.inputTest && g.app.dock.Active() {
		owner = "docking"
	}
	if !g.inputTest && owner == "agent" {
		if st := g.app.act.snapshot(motion.Pose{}); st.Kind != "" {
			owner = st.Kind
		}
	}
	badge, badgeCol := strings.ToUpper(owner), colDim
	switch owner {
	case "manual":
		badgeCol = colAccent
	case "agent", "move", "patrol":
		badgeCol = colAgent
	case "docking":
		badgeCol = colWarn
	case "manual-stop":
		badge, badgeCol = "STOP", colBad
	case "stopped", "":
		badge = "IDLE"
	}
	x, y, pw, ph := 24.0, 1080-24-190.0, 470.0, 190.0
	h.panel(screen, x, y, pw, ph, colPanel)
	h.brackets(screen, x, y, pw, ph, 16, 2, withAlpha(badgeCol, 160))
	h.text(screen, "CONTROL", 13, true, x+20, y+26, colDim, text.AlignStart)
	h.glow(screen, badge, 24, x+20, y+56, badgeCol, text.AlignStart)
	src := g.inputStatus.Source
	if src == "" {
		src = "Keyboard"
	}
	h.text(screen, h.fit(src, 14, false, 170), 14, false, x+20, y+84, colDim, text.AlignStart)

	h.text(screen, "MAX SPEED", 13, true, x+20, y+118, colDim, text.AlignStart)
	speed := g.inputStatus.Speed
	steps := int(math.Round((speed-0.1)/0.1)) + 1
	for i := 0; i < 3; i++ {
		c := color.Color(withAlpha(colText, 30))
		if speed > 0 && i < steps {
			c = colAccent
		}
		h.fill(screen, x+20+float64(i)*44, y+134, 38, 10, c)
	}
	h.mono(screen, fmt.Sprintf("%.1f", speed), 16, x+20+3*44+4, y+139, colText, text.AlignStart)
	if !g.inputTest {
		label, c := "FRONT  —", colDim
		if m, ok := h.heldRange(g, time.Now()); ok {
			label, c = "FRONT  clear", colDim
			if !math.IsInf(m, 1) {
				label = "FRONT  " + fmtRange(m)
				if m < motion.GuardSlow {
					c = rangeColor(m)
				}
			}
		}
		h.mono(screen, label, 14, x+20, y+170, c, text.AlignStart)
	}

	m := state.Motion
	moving := m.X != 0 || m.Y != 0 || m.Z != 0
	h.drawSpeedGauge(screen, x+262, y+92, 54, math.Hypot(m.X, m.Y), 0.3, moving)

	// Motion pad: dot = translation (up is forward), bar = turn.
	cx, cy, r := x+pw-70, y+80, 46.0
	h.ring(screen, cx, cy, r, 1.5, withAlpha(colText, 40))
	h.line(screen, cx-r, cy, cx+r, cy, 1, withAlpha(colText, 30))
	h.line(screen, cx, cy-r, cx, cy+r, 1, withAlpha(colText, 30))
	dx := math.Max(-1, math.Min(1, -m.Y/0.3)) * r
	dy := math.Max(-1, math.Min(1, -m.X/0.3)) * r
	dotCol := colDim
	if moving {
		dotCol = colAccent
	}
	h.line(screen, cx, cy, cx+dx, cy+dy, 3, dotCol)
	h.dot(screen, cx+dx, cy+dy, 8, dotCol)
	turn := math.Max(-1, math.Min(1, -m.Z/1.8)) * r
	h.fill(screen, cx-r, cy+r+14, 2*r, 6, withAlpha(colText, 30))
	if turn != 0 {
		h.fill(screen, math.Min(cx, cx+turn), cy+r+14, math.Abs(turn), 6, colAccent)
	}
	h.text(screen, "TURN", 12, true, cx, cy+r+32, colDim, text.AlignCenter)
}

var dockSteps = []string{"SEARCH", "ALIGN", "REVERSE", "CHARGING"}

func (h *hud) dockVisible(op docking.Operation, now time.Time) bool {
	return op.ID != "" && (op.Active || now.Sub(op.UpdatedAt) <= 20*time.Second)
}

func (h *hud) drawDock(screen *ebiten.Image, op docking.Operation, w float64, now time.Time) {
	if !h.dockVisible(op, now) {
		return
	}
	pw, ph := 680.0, 150.0
	x, y := (w-pw)/2, 1080-24-ph
	h.panel(screen, x, y, pw, ph, colPanel)
	stateCol, stateText := colAccent, strings.ToUpper(strings.ReplaceAll(op.State, "_", " "))
	switch op.State {
	case "charging":
		stateCol, stateText = colGood, "DOCKED"
	case "failed", "timed_out", "cancel_failed":
		stateCol = colBad
	case "cancelled":
		stateCol = colDim
	}
	h.text(screen, "AUTO DOCK", 20, true, x+22, y+30, colText, text.AlignStart)
	h.text(screen, stateText, 20, true, x+22+h.width("AUTO DOCK", 20, true)+18, y+30, stateCol, text.AlignStart)
	end := now
	if !op.Active {
		end = op.UpdatedAt
	}
	timer := fmtDuration(end.Sub(op.StartedAt)) + " / " + fmtDuration(op.Deadline.Sub(op.StartedAt))
	h.text(screen, timer, 18, false, x+pw-22, y+30, colDim, text.AlignEnd)

	current := -1
	switch op.FirmwarePhase {
	case 1, 8:
		current = 0
	case 2:
		current = 1
	case 3, 4:
		current = 2
	}
	if op.State == "charging" {
		current = 4
	}
	failed := !op.Active && op.State != "charging"
	sw := (pw - 44 - 3*10) / 4
	for i, name := range dockSteps {
		sx := x + 22 + float64(i)*(sw+10)
		bg, fg := color.Color(color.NRGBA{255, 255, 255, 14}), color.Color(colDim)
		switch {
		case i < current:
			bg, fg = color.NRGBA{61, 220, 132, 60}, colGood
		case i == current && failed:
			bg, fg = color.NRGBA{255, 77, 77, 70}, colBad
		case i == current:
			bg, fg = color.NRGBA{79, 209, 255, 70}, colText
		}
		h.panel(screen, sx, y+54, sw, 40, bg)
		h.text(screen, name, 16, true, sx+sw/2, y+74, fg, text.AlignCenter)
	}
	line, lineCol := "", colDim
	switch {
	case op.Error != "":
		line, lineCol = op.Error, colBad
	case op.State == "charging":
		line, lineCol = "Docked and charging (verified over SSH)", colGood
	case op.Active:
		line = "X or Xbox B cancels"
		if op.Power.Status != "" {
			line += " · power: " + op.Power.Status
		}
	}
	h.text(screen, h.fit(line, 17, false, pw-44), 17, false, x+22, y+124, lineCol, text.AlignStart)
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func (h *hud) drawNotices(screen *ebiten.Image, g *Game, w float64, now time.Time) {
	y := 160.0
	if g.app.ready.Load() || g.inputTest {
		y = h.noticeY
	}
	for _, n := range g.app.notices.current(now) {
		msg := h.fit(n.text, 19, false, 900)
		tw := h.width(msg, 19, false) + 56
		h.panel(screen, (w-tw)/2, y, tw, 44, colPanel)
		c := colAccent
		switch n.kind {
		case noticeGood:
			c = colGood
		case noticeBad:
			c = colBad
		}
		h.fill(screen, (w-tw)/2+12, y+12, 5, 20, c)
		h.text(screen, msg, 19, false, w/2+8, y+22, colText, text.AlignCenter)
		y += 52
	}
}

func (h *hud) drawHelp(screen *ebiten.Image, g *Game, w float64) {
	if !g.showHelp {
		hint := "?  Controls"
		tw := h.width(hint, 16, false) + 32
		h.panel(screen, w-24-tw, 1080-24-40, tw, 40, colPanel)
		h.text(screen, hint, 16, false, w-24-tw/2, 1080-24-20, colDim, text.AlignCenter)
		return
	}
	rows := [][3]string{
		{"Drive and turn", "W A S D / arrows", "Left stick"},
		{"Strafe", "Q / E", "Right stick"},
		{"Stop / cancel docking", "X", "B or LB + RB"},
		{"Speed down / up", "O / P", "LB / RB"},
		{"Dock", "H", "Y"},
		{"Screenshot", "Space", "A"},
		{"Light dim / bright", "9 / 0", "D-pad down / up"},
		{"Automatic light", "-", "X"},
		{"Record video on the Mac", "R", "-"},
		{"Guard mode on / off", "G", "-"},
		{"Clean HUD / full screen", "Tab / F", "-"},
		{"Show / hide this help", "?", "-"},
		{"Robot menu: patrols, camera, health…", "M", "-"},
		{"Voice assistant on / off", "V", "-"},
		{"Quit", "Esc", "Menu"},
	}
	pw, ph := 940.0, 110+float64(len(rows))*38
	x, y := (w-pw)/2, (1080-ph)/2
	h.panel(screen, x, y, pw, ph, color.NRGBA{12, 14, 18, 235})
	h.text(screen, "Controls", 26, true, x+32, y+40, colText, text.AlignStart)
	cols := []float64{x + 32, x + 420, x + 700}
	for i, head := range []string{"ACTION", "KEYBOARD", "XBOX"} {
		h.text(screen, head, 14, true, cols[i], y+80, colDim, text.AlignStart)
	}
	for r, row := range rows {
		ry := y + 116 + float64(r)*38
		for i, cell := range row {
			c := colText
			if i > 0 {
				c = colAccent
			}
			h.text(screen, cell, 18, i > 0, cols[i], ry, c, text.AlignStart)
		}
	}
}

func (h *hud) drawConnect(screen *ebiten.Image, g *Game, w float64) {
	phase, errText, t := g.app.connState()
	note := g.app.note()
	scanning, found, sel := g.finder.snapshot()
	noteLines := h.wrap(note, 18, 680, 5)
	listRows := min(len(found), 6)
	pw := 760.0
	ph := 440 + float64(len(noteLines))*26 + float64(listRows)*34
	if scanning || len(found) > 0 {
		ph += 40
	}
	x, y := (w-pw)/2, (1080-ph)/2
	h.panel(screen, x, y, pw, ph, color.NRGBA{24, 27, 33, 255})
	h.text(screen, "Scout", 44, true, x+40, y+60, colText, text.AlignStart)
	h.text(screen, "Connect to your robot", 22, false, x+40, y+104, colDim, text.AlignStart)
	cy := y + 140
	for _, line := range noteLines {
		h.text(screen, line, 18, false, x+40, cy, colAccent, text.AlignStart)
		cy += 26
	}
	if len(noteLines) > 0 {
		cy += 10
	}
	h.text(screen, "ROBOT IP OR HOSTNAME", 14, true, x+40, cy+10, colDim, text.AlignStart)

	field := g.host.text
	if phase == connConnecting {
		field = t.Host
	}
	border := colFaint
	if phase != connConnecting {
		border = colAccent
	}
	fy := cy + 28
	h.panel(screen, x+40, fy, pw-80, 64, color.NRGBA{0, 0, 0, 120})
	vector.StrokeRect(screen, float32((x+40)*h.s), float32(fy*h.s), float32((pw-80)*h.s), float32(64*h.s), float32(2*h.s), border, true)
	shown := field
	if phase != connConnecting && time.Now().UnixMilli()/530%2 == 0 {
		shown += "|"
	}
	if field == "" && phase != connConnecting {
		h.text(screen, "e.g. 192.168.1.97", 28, false, x+62, fy+32, colFaint, text.AlignStart)
	}
	h.text(screen, shown, 28, false, x+62, fy+32, colText, text.AlignStart)

	host := strings.TrimSpace(field)
	if host == "" {
		host = "<host>"
	}
	sshNote := "SSH as root@" + host + " with your SSH keys · saved for next time"
	if *flagNoSSH {
		sshNote = "SSH disabled (-no-ssh) · docking and speaker unavailable"
	}
	cy = fy + 90
	h.text(screen, h.fit(sshNote, 17, false, pw-80), 17, false, x+40, cy, colDim, text.AlignStart)
	cy += 40

	if scanning || len(found) > 0 {
		head := fmt.Sprintf("FOUND %d ROBOT ADDRESS(ES) - Up/Down to choose", len(found))
		if scanning {
			head = "SEARCHING THIS NETWORK" + strings.Repeat(".", int(time.Now().UnixMilli()/400%4))
		}
		h.text(screen, head, 14, true, x+40, cy, colDim, text.AlignStart)
		cy += 30
		for i := 0; i < listRows; i++ {
			c := colText
			if i == sel {
				h.panel(screen, x+36, cy-16, pw-72, 32, color.NRGBA{79, 209, 255, 50})
				c = colAccent
			}
			label := found[i]
			if found[i] == hotspotAddress {
				label += "   (robot hotspot)"
			}
			h.text(screen, label, 19, i == sel, x+52, cy, c, text.AlignStart)
			cy += 34
		}
		cy += 6
	}

	switch phase {
	case connConnecting:
		dots := strings.Repeat(".", int(time.Now().UnixMilli()/400%4))
		h.text(screen, "Connecting to "+t.Host+dots, 20, true, x+40, cy+8, colWarn, text.AlignStart)
	case connFailed:
		for i, line := range h.wrap(errText, 18, pw-80, 3) {
			h.text(screen, line, 18, false, x+40, cy+float64(i)*26, colBad, text.AlignStart)
		}
	}
	h.text(screen, "Enter  connect      Tab  find robots      Esc  quit", 17, false, x+40, y+ph-30, colDim, text.AlignStart)
	if ts, lan := g.app.phones.snapshot(); ts != "" || len(lan) > 0 {
		addr := ts
		if addr == "" {
			addr = lan[0]
		}
		h.text(screen, "Phones and tablets: "+addr, 17, false, w/2, y+ph+30, colDim, text.AlignCenter)
	}
}

func (h *hud) drawMenu(screen *ebiten.Image, g *Game, w float64) {
	m := &g.menu
	if m.page == menuClosed {
		if g.app.ready.Load() && !g.inputTest {
			hint := "M  Robot menu"
			tw := h.width(hint, 16, false) + 32
			h.panel(screen, w-24-tw, 1080-24-40-52, tw, 40, colPanel)
			h.text(screen, hint, 16, false, w-24-tw/2, 1080-24-72, colDim, text.AlignCenter)
		}
		return
	}
	scanning, nets, status, bad := m.snapshot()
	pw, ph := 820.0, 600.0
	switch m.page {
	case menuRestore:
		pw, ph = 900, 800
	case menuMain:
		pw, ph = 900, 120+float64(len(menuItems))*62+90
	}
	if fw, fh, ok := featureMenuSize(m.page); ok {
		pw, ph = fw, fh
	}
	x, y := (w-pw)/2, (1080-ph)/2
	h.panel(screen, x, y, pw, ph, color.NRGBA{8, 13, 20, 245})
	h.brackets(screen, x, y, pw, ph, 26, 2.5, withAlpha(colAccent, 180))
	title, footer := "Robot", "Up/Down choose      Enter open / toggle      Left/Right adjust      Esc close"
	cy := y + 110
	row := func(label, detail string, selected, enabled bool) {
		if selected {
			h.panel(screen, x+24, cy-26, pw-48, 64, color.NRGBA{79, 209, 255, 45})
		}
		c := colText
		if !enabled {
			c = colDim
		}
		h.text(screen, label, 21, true, x+44, cy-6, c, text.AlignStart)
		if detail != "" {
			h.text(screen, h.fit(detail, 16, false, pw-100), 16, false, x+44, cy+18, colDim, text.AlignStart)
		}
		cy += 74
	}
	field := func(label string, f *textField, focused, secret bool) {
		h.text(screen, label, 14, true, x+40, cy, colDim, text.AlignStart)
		cy += 18
		border := colFaint
		if focused {
			border = colAccent
		}
		h.panel(screen, x+40, cy, pw-80, 56, color.NRGBA{0, 0, 0, 120})
		vector.StrokeRect(screen, float32((x+40)*h.s), float32(cy*h.s), float32((pw-80)*h.s), float32(56*h.s), float32(2*h.s), border, true)
		shown := f.text
		if secret {
			shown = strings.Repeat("•", len([]rune(f.text)))
		}
		if focused && time.Now().UnixMilli()/530%2 == 0 {
			shown += "|"
		}
		h.text(screen, h.fit(shown, 24, false, pw-130), 24, false, x+60, cy+28, colText, text.AlignStart)
		cy += 84
	}
	para := func(s string, c color.Color) {
		for _, line := range h.wrap(s, 18, pw-80, 6) {
			h.text(screen, line, 18, false, x+40, cy, c, text.AlignStart)
			cy += 28
		}
		cy += 10
	}
	switch m.page {
	case menuMain:
		cy = y + 100
		for i, it := range menuItems {
			detail := it.detail
			switch i {
			case miAutoDock:
				ad := g.app.autoDockSettings()
				detail = fmt.Sprintf("%s · docks below %d%% while this app runs · Enter on/off, Left/Right threshold", strings.ToUpper(onOff(ad.Enabled)), ad.Threshold)
			case miObstacle:
				detail = fmt.Sprintf("%s for manual driving · slows from %.1f m, stops at %.2f m · moves and patrols are always guarded",
					strings.ToUpper(onOff(g.app.manualObstacleGuard())), motion.GuardSlow, motion.GuardStop)
			case miGuard:
				if g.app.guardSettings().Armed {
					detail = "ARMED · " + detail
				}
			case miRoutes:
				if n, _, _ := g.app.routes.recordingRoute(); n != "" {
					detail = "Recording " + n + " now · open to save it"
				} else if c := len(g.app.routes.List()); c > 0 {
					detail = fmt.Sprintf("%d saved · %s", c, detail)
				}
			}
			if i == m.sel {
				h.panel(screen, x+24, cy-24, pw-48, 56, color.NRGBA{79, 209, 255, 45})
				h.fill(screen, x+24, cy-24, 4, 56, colAccent)
			}
			c := colText
			if it.ssh && g.app.audio == nil {
				c = colDim
			}
			h.text(screen, it.title, 19, true, x+44, cy-6, c, text.AlignStart)
			h.text(screen, h.fit(detail, 14, false, pw-100), 14, false, x+44, cy+15, colDim, text.AlignStart)
			cy += 62
		}
	case menuWiFiList:
		title, footer = "Wi-Fi network", "Up/Down choose      Enter select      R rescan      Esc back"
		start := max(0, min(m.sel-4, len(nets)+1-6))
		for i := start; i < min(len(nets)+1, start+6); i++ {
			if i == len(nets) {
				row("Other network…", "Type a network name, e.g. a hidden network", i == m.sel, true)
				continue
			}
			n := nets[i]
			band := "2.4 GHz"
			if n.Freq >= 4.9 || n.Freq > 4900 {
				band = "5 GHz"
			}
			row(n.SSID, fmt.Sprintf("%d dBm · %s · channel %d", n.Signal, band, n.Channel), i == m.sel, true)
		}
		if scanning && len(nets) == 0 {
			cy -= 74
		}
	case menuWiFiCredentials:
		title, footer = "Wi-Fi network", "Enter continue      Hold Ctrl to show password      Esc back"
		if m.custom {
			footer = "Enter continue      Tab next field      Hold Ctrl to show password      Esc back"
			field("NETWORK NAME", &m.ssid, m.field == 0, false)
		} else {
			h.text(screen, "NETWORK", 14, true, x+40, cy, colDim, text.AlignStart)
			h.text(screen, m.ssid.text, 24, true, x+40, cy+34, colText, text.AlignStart)
			cy += 80
		}
		field("PASSWORD (empty for an open network)", &m.pass, m.field == 1, !ebiten.IsKeyPressed(ebiten.KeyControl))
	case menuWiFiConfirm:
		title, footer = "Join "+m.ssid.text+"?", "Enter join      Esc back"
		para(fmt.Sprintf("The robot will leave its current network and join %q. This session will disconnect.", m.ssid.text), colText)
		para(fmt.Sprintf("If it isn't connected within %d minutes, the robot restores its previous Wi-Fi and reboots, so a wrong password can't strand it.", int(agent.WiFiRevertAfter/time.Minute)), colDim)
		para("Afterwards, put this Mac on the same network and press Tab on the connect screen to find the robot.", colDim)
	case menuReset:
		title, footer = "Reset robot settings?", "Y reset      Esc cancel"
		para("Runs the robot's own settings reset, then reboots it into hotspot mode.", colText)
		para("Deletes: Wi-Fi settings, Moorebot cloud link, timezone, maps and patrols, recordings and caches. Restores default video, sound and monitoring settings.", colDim)
		para("Keeps: the operating system, SSH keys and root password. You can reconnect over the robot's hotspot at "+hotspotAddress+".", colDim)
	case menuPhone:
		title, footer = "Phone & tablet", "T Tailscale    L home Wi-Fi    N new code    B open here    C copy full key    Esc back"
		m.mu.Lock()
		pa, checking := m.phone, m.checking
		m.mu.Unlock()
		tx := x + 40
		switch {
		case g.app.apiToken == "":
			para("The web app isn't running (started with -api off, or its port was busy).", colBad)
		case checking && pa.DNSName == "":
			para("Checking Tailscale…", colDim)
		case pa.URL != "" || len(lanURLs(g)) > 0:
			url := pa.URL
			if url == "" {
				url = lanURLs(g)[0]
			}
			pair := url + "#pair=" + g.app.apiToken
			if img := h.qr(pair); img != nil {
				op := &ebiten.DrawImageOptions{}
				size := 300.0
				op.GeoM.Scale(size*h.s/float64(img.Bounds().Dx()), size*h.s/float64(img.Bounds().Dy()))
				op.GeoM.Translate(tx*h.s, cy*h.s)
				screen.DrawImage(img, op)
				tx += size + 30
			}
			ty := cy + 14
			for _, line := range []struct {
				s string
				c color.Color
				b bool
			}{
				{"Scan with the iPhone or iPad camera", colText, true},
				{"Opens Scout in Safari, already paired.", colDim, false},
				{"Then Share > Add to Home Screen for a full-screen app.", colDim, false},
				{"", colDim, false},
				{tsLabel(pa.URL), colDim, true},
				{tsValue(pa.URL), colAccent, false},
				{"HOME WI-FI (L to turn " + onOff(!lanOn(g)) + ")", colDim, true},
				{lanValue(g), colAccent, false},
				{"", colDim, false},
				{"Or open the address and enter this one-time code:", colDim, false},
			} {
				if line.s != "" {
					h.text(screen, h.fit(line.s, 17, line.b, x+pw-40-tx), 17, line.b, tx, ty, line.c, text.AlignStart)
				}
				ty += 28
			}
			if code, exp := g.app.codes.Current(); code != "" {
				h.text(screen, code[:3]+" "+code[3:], 40, true, tx, ty+16, colText, text.AlignStart)
				h.text(screen, fmt.Sprintf("valid %s, once", fmtDuration(time.Until(exp))), 15, false, tx+200, ty+18, colDim, text.AlignStart)
			}
			cy += 340
		case !pa.Tailscale:
			para("Tailscale is needed to reach Scout from a phone or tablet.", colText)
			if pa.Err != "" {
				para(pa.Err, colBad)
			}
			para("Install Tailscale on this Mac and on the iPhone/iPad, sign in to the same account, then press R.", colDim)
			para("Or press L to allow phones on your home Wi-Fi (unencrypted HTTP on your network; devices still pair).", colDim)
			para("On this Mac you can always press B to open Scout in a browser.", colDim)
		default:
			para("Tailscale is running on this Mac ("+pa.DNSName+"), but Scout isn't shared on your tailnet yet.", colText)
			para("Press T to run: tailscale serve --bg --https="+pa.Port+" http://"+*flagAPI, colAccent)
			para("Or press L to allow phones on your home Wi-Fi (unencrypted HTTP; devices still pair).", colDim)
			para("This gives Scout a private HTTPS address only your own Tailscale devices can reach. Other services you already share are left alone.", colDim)
		}
	case menuRestore:
		title, footer = "Factory restore", "Type RESTORE, then Enter      Esc cancel"
		m.mu.Lock()
		checks, checking := m.checks, m.checking
		m.mu.Unlock()
		para("Reinstalls the robot's system from the factory image on its backup partition, then reboots into hotspot mode.", colText)
		para("Erases: SSH keys and root password (back to factory), Wi-Fi, all settings and anything installed on the system. Recordings in /userdata are kept.", colWarn)
		if checking {
			para("Checking the robot…", colDim)
		}
		labels := map[string]string{"image:update.tar.xz": "Factory image", "image:backup.sh": "Restore script", "image:recovery_status": "Recovery marker",
			"ota": "No pending OTA", "misc": "Boot message free", "power": "On the charger", "battery": "Battery"}
		for _, c := range checks {
			mark, mc := "OK", colGood
			if !c.OK {
				mark, mc = "NO", colBad
			}
			name := labels[c.Name]
			if name == "" {
				name = c.Name
			}
			h.text(screen, mark, 15, true, x+40, cy, mc, text.AlignStart)
			h.text(screen, name, 16, true, x+86, cy, colText, text.AlignStart)
			h.text(screen, h.fit(c.Info, 15, false, pw-330), 15, false, x+290, cy, colDim, text.AlignStart)
			cy += 26
		}
		cy += 8
		if agent.RestoreReady(checks) {
			field("TYPE RESTORE TO CONFIRM", &m.confirm, true, false)
		}
	case menuBusy:
		title, footer = "Working…", ""
		if bad {
			footer = "Esc back"
		}
	default:
		title, footer = h.drawFeature(screen, g, x, y, pw, ph)
	}
	h.text(screen, title, 28, true, x+40, y+52, colText, text.AlignStart)
	if status != "" {
		c := colAccent
		if bad {
			c = colBad
		}
		for i, line := range h.wrap(status, 18, pw-80, 3) {
			h.text(screen, line, 18, false, x+40, y+ph-110+float64(i)*26, c, text.AlignStart)
		}
	}
	if footer != "" {
		h.text(screen, footer, 16, false, x+40, y+ph-30, colDim, text.AlignStart)
	}
}

// qr renders text as a QR code image, cached for the last text.
func (h *hud) qr(s string) *ebiten.Image {
	if s == h.qrText && h.qrImage != nil {
		return h.qrImage
	}
	code, err := qr.Encode(s, qr.M)
	if err != nil {
		return nil
	}
	code.Scale = 8
	if h.qrImage != nil {
		h.qrImage.Deallocate()
	}
	h.qrText, h.qrImage = s, ebiten.NewImageFromImage(code.Image())
	return h.qrImage
}

func lanOn(g *Game) bool {
	g.app.settingsMu.Lock()
	defer g.app.settingsMu.Unlock()
	return g.app.settings.LANAccess
}

func lanURLs(g *Game) []string {
	_, lan := g.app.phones.snapshot()
	return lan
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func tsLabel(url string) string {
	if url == "" {
		return "TAILSCALE (T to share)"
	}
	return "TAILSCALE (any network)"
}

func tsValue(url string) string {
	if url == "" {
		return "not shared"
	}
	return url
}

func lanValue(g *Game) string {
	if lan := lanURLs(g); len(lan) > 0 {
		return strings.Join(lan, "  ")
	}
	if lanOn(g) {
		return "no private Wi-Fi address found"
	}
	return "off"
}

// drawPhonePanel keeps the phone/tablet address visible in the HUD.
func (h *hud) drawPhonePanel(screen *ebiten.Image, g *Game, w float64) {
	if g.app.apiToken == "" {
		return
	}
	ts, lan := g.app.phones.snapshot()
	type line struct{ label, value string }
	var lines []line
	if ts != "" {
		lines = append(lines, line{"PHONE", ts})
	}
	for _, u := range lan {
		lines = append(lines, line{"WI-FI", u})
	}
	off := len(lines) == 0
	if off {
		// Not reachable yet: still show the address it would use and how to turn it on.
		_, port, _ := net.SplitHostPort(*flagAPI)
		for _, ip := range privateIPv4s() {
			lines = append(lines, line{"WI-FI", "http://" + net.JoinHostPort(ip, port) + "  (off: M > Phone & tablet > L)"})
		}
		if len(lines) == 0 {
			lines = append(lines, line{"PHONE", "M > Phone & tablet to set up"})
		}
	}
	if code, exp := g.app.codes.Current(); code != "" && !off {
		lines = append(lines, line{"CODE", code[:3] + " " + code[3:] + "  (one-time, " + fmtDuration(time.Until(exp)) + ")"})
	}
	pw := 0.0
	for _, l := range lines {
		pw = math.Max(pw, h.width(l.value, 16, false))
	}
	pw += 110
	ph := 16 + float64(len(lines))*26
	x, y := w-24-pw, 1080-24-40-12-40-12-ph
	h.panel(screen, x, y, pw, ph, colPanel)
	for i, l := range lines {
		ly := y + 21 + float64(i)*26
		h.text(screen, l.label, 13, true, x+14, ly, colDim, text.AlignStart)
		c := colAccent
		if off {
			c = colDim
		}
		h.text(screen, l.value, 16, false, x+96, ly, c, text.AlignStart)
	}
}

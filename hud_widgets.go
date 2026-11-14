package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
	"github.com/hajimehoshi/ebiten/v2/vector"

	"github.com/gmkbenjamin/scoutdeck/internal/events"
	"github.com/gmkbenjamin/scoutdeck/internal/motion"
)

// Drawing helpers in 1080p units.

func (h *hud) line(dst *ebiten.Image, x0, y0, x1, y1, width float64, c color.Color) {
	vector.StrokeLine(dst, float32(x0*h.s), float32(y0*h.s), float32(x1*h.s), float32(y1*h.s), float32(width*h.s), c, true)
}

func (h *hud) dot(dst *ebiten.Image, x, y, r float64, c color.Color) {
	vector.FillCircle(dst, float32(x*h.s), float32(y*h.s), float32(r*h.s), c, true)
}

func (h *hud) ring(dst *ebiten.Image, x, y, r, width float64, c color.Color) {
	vector.StrokeCircle(dst, float32(x*h.s), float32(y*h.s), float32(r*h.s), float32(width*h.s), c, true)
}

// arc strokes an arc; angles in radians, 0 = right, increasing clockwise on screen.
func (h *hud) arc(dst *ebiten.Image, cx, cy, r, a0, a1, width float64, c color.Color) {
	var p vector.Path
	p.Arc(float32(cx*h.s), float32(cy*h.s), float32(r*h.s), float32(a0), float32(a1), vector.Clockwise)
	op := &vector.DrawPathOptions{AntiAlias: true}
	op.ColorScale.ScaleWithColor(c)
	vector.StrokePath(dst, &p, &vector.StrokeOptions{Width: float32(width * h.s), LineCap: vector.LineCapRound}, op)
}

// brackets draws corner brackets around a rectangle.
func (h *hud) brackets(dst *ebiten.Image, x, y, w, ht, l, width float64, c color.Color) {
	l = math.Min(l, math.Min(w, ht)/2)
	for _, k := range [][4]float64{
		{x, y, 1, 1}, {x + w, y, -1, 1}, {x, y + ht, 1, -1}, {x + w, y + ht, -1, -1},
	} {
		h.line(dst, k[0], k[1], k[0]+k[2]*l, k[1], width, c)
		h.line(dst, k[0], k[1], k[0], k[1]+k[3]*l, width, c)
	}
}

// mono draws monospaced text (numeric readouts).
func (h *hud) mono(dst *ebiten.Image, s string, size float64, x, y float64, c color.Color, align text.Align) {
	op := &text.DrawOptions{}
	op.GeoM.Translate(x*h.s, y*h.s)
	op.ColorScale.ScaleWithColor(c)
	op.PrimaryAlign = align
	op.SecondaryAlign = text.AlignCenter
	text.Draw(dst, s, h.monoFace(size), op)
}

func (h *hud) monoWidth(s string, size float64) float64 {
	w, _ := text.Measure(s, h.monoFace(size), 0)
	return w / h.s
}

func (h *hud) monoFace(size float64) *text.GoTextFace {
	px := int(math.Round(size * h.s))
	key := [2]int{px, 2}
	f, ok := h.faces[key]
	if !ok {
		f = &text.GoTextFace{Source: h.monoSrc, Size: float64(px)}
		h.faces[key] = f
	}
	return f
}

// glow draws bold text with a soft halo.
func (h *hud) glow(dst *ebiten.Image, s string, size float64, x, y float64, c color.NRGBA, align text.Align) {
	halo := color.NRGBA{c.R, c.G, c.B, 40}
	for _, d := range [][2]float64{{-1.5, 0}, {1.5, 0}, {0, -1.5}, {0, 1.5}} {
		h.text(dst, s, size, true, x+d[0], y+d[1], halo, align)
	}
	h.text(dst, s, size, true, x, y, c, align)
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA { return color.NRGBA{c.R, c.G, c.B, a} }

func lerpColor(a, b color.NRGBA, t float64) color.NRGBA {
	t = math.Max(0, math.Min(1, t))
	m := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.NRGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), m(a.A, b.A)}
}

// rangeColor maps an obstacle distance to green, amber or red.
func rangeColor(m float64) color.NRGBA {
	switch {
	case m <= motion.GuardStop+0.07:
		return colBad
	case m < motion.GuardSlow:
		return colWarn
	case m < 1.2:
		return lerpColor(colWarn, colGood, (m-motion.GuardSlow)/0.6)
	}
	return colGood
}

// ---------- Screen treatment ----------

// overlay caches a 1-pixel-wide vignette strip, stretched to the screen.
type overlay struct {
	h       int
	vignete *ebiten.Image
}

func (o *overlay) ensure(height int) {
	if o.h == height && o.vignete != nil {
		return
	}
	if o.vignete != nil {
		o.vignete.Deallocate()
	}
	o.h = height
	v := image.NewNRGBA(image.Rect(0, 0, 1, height))
	for y := 0; y < height; y++ {
		t := float64(y) / float64(height)
		edge := math.Max(0, math.Max(0.22-t, t-0.72)) / 0.25 // top 22%, bottom 28%
		v.SetNRGBA(0, y, color.NRGBA{0, 4, 10, uint8(math.Min(1, edge) * 170)})
	}
	o.vignete = ebiten.NewImageFromImage(v)
}

func (h *hud) drawOverlay(screen *ebiten.Image, w float64) {
	H := screen.Bounds().Dy()
	h.over.ensure(H)
	W := float64(screen.Bounds().Dx())
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Scale(W, 1)
	screen.DrawImage(h.over.vignete, op)
	// Frame brackets around the whole view.
	h.brackets(screen, 14, 76, w-28, 1080-90, 46, 2.5, withAlpha(colAccent, 120))
}

// ---------- Reticle ----------

// drawReticle is a plain, static crosshair. When the obstacle guard is
// actually slowing the robot, a steady red ring says so.
func (h *hud) drawReticle(screen *ebiten.Image, g *Game, w float64, now time.Time) {
	cx, cy := w/2, 540.0
	c := withAlpha(colText, 110)
	for _, d := range [][4]float64{{-34, 0, -14, 0}, {14, 0, 34, 0}, {0, -34, 0, -14}, {0, 14, 0, 34}} {
		h.line(screen, cx+d[0], cy+d[1], cx+d[2], cy+d[3], 2, c)
	}
	if g.app.sensors.limitedRecently(now) {
		h.ring(screen, cx, cy, 52, 3, withAlpha(colBad, 200))
		h.text(screen, "OBSTACLE AHEAD", 16, true, cx, cy+74, colBad, text.AlignCenter)
	}
}

// heldRange samples the filtered range twice a second so the readout
// changes calmly; it returns ok=false when there is no fresh reading.
func (h *hud) heldRange(g *Game, now time.Time) (float64, bool) {
	if now.Sub(h.rangeAt) >= 500*time.Millisecond {
		h.rangeAt = now
		r := g.app.sensors.rangeReading()
		h.rangeOK, h.rangeM = r.Fresh(now), r.Meters
	}
	return h.rangeM, h.rangeOK
}

// ---------- Mini-map ----------

func (h *hud) drawMiniMap(screen *ebiten.Image, g *Game, w float64, now time.Time) (bottom float64) {
	size := 300.0
	x, y := w-24-size, 84.0
	h.panel(screen, x, y, size, size, color.NRGBA{4, 10, 16, 190})
	cx, cy := x+size/2, y+size/2+20
	pose := g.app.sensors.tracker.Pose()
	trail := g.app.sensors.tracker.Trail()
	// Scale: fit the trail and dock within the panel, at least ±1.5 m.
	reach := 1.5
	for _, p := range append(trail, motion.Point{}) {
		reach = math.Max(reach, math.Hypot(p.X-pose.X, p.Y-pose.Y))
	}
	reach = math.Min(reach, 25)
	scale := (size/2 - 26) / reach // units per metre
	// Heading-up: rotate the world so the robot faces the top of the panel.
	toScreen := func(px, py float64) (float64, float64) {
		dx, dy := px-pose.X, py-pose.Y
		c, s := math.Cos(-pose.Yaw), math.Sin(-pose.Yaw)
		fx, fy := dx*c-dy*s, dx*s+dy*c // fx ahead, fy left
		return cx - fy*scale, cy - fx*scale
	}
	// Range rings every metre (or 5 m when zoomed out).
	step := 1.0
	if reach > 6 {
		step = 5
	}
	for m := step; m*scale < size/2+40; m += step {
		h.ring(screen, cx, cy, m*scale, 1, withAlpha(colAccent, 30))
	}
	// Time-of-flight cone.
	if held, ok := h.heldRange(g, now); ok {
		r := motion.Range{Meters: held}
		reachM := 2.0
		col := withAlpha(colAccent, 30)
		if !math.IsInf(r.Meters, 1) {
			reachM, col = r.Meters, withAlpha(colAccent, 60)
			if r.Meters < motion.GuardSlow {
				col = withAlpha(rangeColor(r.Meters), 90)
			}
		}
		half := 0.218 // half the 25° field of view
		L := math.Min(reachM*scale, size/2)
		var p vector.Path
		p.MoveTo(float32(cx*h.s), float32(cy*h.s))
		p.LineTo(float32((cx-math.Sin(half)*L)*h.s), float32((cy-math.Cos(half)*L)*h.s))
		p.LineTo(float32((cx+math.Sin(half)*L)*h.s), float32((cy-math.Cos(half)*L)*h.s))
		p.Close()
		op := &vector.DrawPathOptions{AntiAlias: true}
		op.ColorScale.ScaleWithColor(col)
		vector.FillPath(screen, &p, nil, op)
		if !math.IsInf(r.Meters, 1) && r.Meters < motion.GuardSlow {
			h.line(screen, cx-math.Sin(half)*L, cy-math.Cos(half)*L, cx+math.Sin(half)*L, cy-math.Cos(half)*L, 3, rangeColor(r.Meters))
		}
	}
	// Trail, fading with age.
	inside := func(px, py float64) bool { return px > x+4 && px < x+size-4 && py > y+4 && py < y+size-4 }
	for i := 1; i < len(trail); i++ {
		x0, y0 := toScreen(trail[i-1].X, trail[i-1].Y)
		x1, y1 := toScreen(trail[i].X, trail[i].Y)
		if !inside(x0, y0) || !inside(x1, y1) {
			continue
		}
		a := uint8(50 + 170*float64(i)/float64(len(trail)))
		h.line(screen, x0, y0, x1, y1, 2.5, withAlpha(colAccent, a))
	}
	// Dock at the origin.
	if dx, dy := toScreen(0, 0); inside(dx, dy) {
		h.fill(screen, dx-7, dy-7, 14, 14, withAlpha(colGood, 200))
		h.text(screen, "DOCK", 11, true, dx, dy+16, colGood, text.AlignCenter)
	}
	// Robot.
	var p vector.Path
	p.MoveTo(float32(cx*h.s), float32((cy-13)*h.s))
	p.LineTo(float32((cx+9)*h.s), float32((cy+9)*h.s))
	p.LineTo(float32(cx*h.s), float32((cy+4)*h.s))
	p.LineTo(float32((cx-9)*h.s), float32((cy+9)*h.s))
	p.Close()
	op := &vector.DrawPathOptions{AntiAlias: true}
	op.ColorScale.ScaleWithColor(colText)
	vector.FillPath(screen, &p, nil, op)
	h.text(screen, "MAP", 13, true, x+14, y+18, colDim, text.AlignStart)
	src := "gyro + odometry"
	if pose.Source != "odometry" {
		src = "gyro + commands"
	}
	h.text(screen, src, 12, false, x+size-14, y+18, colDim, text.AlignEnd)
	h.mono(screen, fmt.Sprintf("%.1f m travelled", pose.Odometer), 13, x+14, y+size-16, colDim, text.AlignStart)
	h.mono(screen, fmt.Sprintf("%.0f m grid", step), 13, x+size-14, y+size-16, colDim, text.AlignEnd)
	return y + size
}

// ---------- Detection boxes on the video ----------

func (h *hud) drawDetections(screen *ebiten.Image, g *Game, now time.Time) {
	vr := h.video
	if vr.Dx() == 0 {
		return
	}
	vx, vy, vw, vh := float64(vr.Min.X)/h.s, float64(vr.Min.Y)/h.s, float64(vr.Dx())/h.s, float64(vr.Dy())/h.s
	box := func(b [4]float64, label string, c color.NRGBA) {
		x0, y0 := vx+b[0]*vw, vy+b[1]*vh
		bw, bh := (b[2]-b[0])*vw, (b[3]-b[1])*vh
		if bw < 4 || bh < 4 {
			return
		}
		h.brackets(screen, x0, y0, bw, bh, 22, 3, c)
		tw := h.width(label, 15, true) + 16
		h.fill(screen, x0, y0-24, tw, 22, withAlpha(c, 200))
		h.text(screen, label, 15, true, x0+8, y0-13, colBack, text.AlignStart)
	}
	for _, d := range g.app.sensors.detections(now) {
		c := colBad
		if d.Name != "person" {
			c = colWarn
		}
		box(d.Box, fmt.Sprintf("%s %.0f%%", strings.ToUpper(d.Name), d.Score*100), c)
	}
	if b := g.app.motionBox(now); b != nil {
		box([4]float64{b.Left, b.Top, b.Right, b.Bottom}, "MOTION", colAgent)
	}
}

// ---------- Event feed with thumbnails ----------

type thumbCache struct {
	imgs map[string]*ebiten.Image
}

func (t *thumbCache) get(g *Game, e events.Event) *ebiten.Image {
	if !e.Snapshot {
		return nil
	}
	if t.imgs == nil {
		t.imgs = map[string]*ebiten.Image{}
	}
	if img, ok := t.imgs[e.ID]; ok {
		return img
	}
	data, err := g.app.events.Snapshot(e.ID)
	var img *ebiten.Image
	if err == nil {
		img, _, _ = ebitenutil.NewImageFromReader(bytes.NewReader(data))
	}
	t.imgs[e.ID] = img // nil is cached too, so a bad file is read once
	return img
}

func (t *thumbCache) keep(ids map[string]bool) {
	for id, img := range t.imgs {
		if !ids[id] {
			if img != nil {
				img.Deallocate()
			}
			delete(t.imgs, id)
		}
	}
}

func eventColor(kind string) color.NRGBA {
	switch kind {
	case "person":
		return colBad
	case "cat", "dog":
		return colWarn
	case "motion":
		return colAgent
	case "patrol", "dock":
		return colGood
	}
	return colAccent
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func (h *hud) drawEventFeed(screen *ebiten.Image, g *Game, w, top float64, now time.Time) float64 {
	list := g.app.events.List(3, now.Add(-6*time.Hour))
	ids := map[string]bool{}
	for _, e := range list {
		ids[e.ID] = true
	}
	h.thumbs.keep(ids)
	if len(list) == 0 {
		return top
	}
	pw := 300.0
	x, y := w-24-pw, top+12
	rowH := 74.0
	h.panel(screen, x, y, pw, 30+rowH*float64(len(list)), color.NRGBA{4, 10, 16, 190})
	h.text(screen, "EVENTS", 13, true, x+14, y+16, colDim, text.AlignStart)
	if g.app.guardSettings().Armed {
		h.text(screen, "GUARD ARMED", 12, true, x+pw-14, y+16, colBad, text.AlignEnd)
	}
	for i, e := range list {
		ry := y + 30 + float64(i)*rowH
		c := eventColor(e.Kind)
		tx := x + 14
		if img := h.thumbs.get(g, e); img != nil {
			tw, th := 96.0, 54.0
			op := &ebiten.DrawImageOptions{Filter: ebiten.FilterLinear}
			op.GeoM.Scale(tw*h.s/float64(img.Bounds().Dx()), th*h.s/float64(img.Bounds().Dy()))
			op.GeoM.Translate((x+12)*h.s, (ry+6)*h.s)
			screen.DrawImage(img, op)
			vector.StrokeRect(screen, float32((x+12)*h.s), float32((ry+6)*h.s), float32(tw*h.s), float32(th*h.s), float32(1.5*h.s), withAlpha(c, 180), true)
			tx = x + 12 + tw + 12
		} else {
			h.fill(screen, x+12, ry+10, 4, 46, c)
			tx = x + 26
		}
		h.text(screen, events.Label(e.Kind), 15, true, tx, ry+20, c, text.AlignStart)
		h.text(screen, ago(now.Sub(e.At)), 13, false, x+pw-14, ry+20, colDim, text.AlignEnd)
		h.text(screen, h.fit(e.Text, 13, false, x+pw-14-tx), 13, false, tx, ry+44, colDim, text.AlignStart)
	}
	return y + 30 + rowH*float64(len(list))
}

// ---------- Activity (move / patrol / route recording) ----------

func (h *hud) drawActivity(screen *ebiten.Image, g *Game, w float64, now time.Time, y float64) bool {
	st := g.app.act.snapshot(g.app.sensors.tracker.Pose())
	routeName, steps, recFor := g.app.routes.recordingRoute()
	pw := 560.0
	x := (w - pw) / 2
	switch {
	case routeName != "":
		h.panel(screen, x, y, pw, 74, colPanel)
		h.dot(screen, x+26, y+26, 7, colBad)
		h.glow(screen, "RECORDING ROUTE", 18, x+44, y+26, colBad, text.AlignStart)
		h.text(screen, routeName, 18, true, x+60+h.width("RECORDING ROUTE", 18, true), y+26, colText, text.AlignStart)
		h.mono(screen, fmtDuration(recFor), 16, x+pw-22, y+26, colDim, text.AlignEnd)
		h.text(screen, fmt.Sprintf("%d steps · drive the route, then M > Patrol routes > Save", steps), 15, false, x+22, y+54, colDim, text.AlignStart)
		return true
	case st.Kind != "":
		h.panel(screen, x, y, pw, 84, colPanel)
		kind := strings.ToUpper(st.Kind)
		h.glow(screen, kind, 18, x+22, y+26, colAgent, text.AlignStart)
		h.text(screen, h.fit(st.Label, 18, true, pw-200), 18, true, x+34+h.width(kind, 18, true), y+26, colText, text.AlignStart)
		h.mono(screen, fmtDuration(time.Duration(st.Seconds*float64(time.Second))), 16, x+pw-22, y+26, colDim, text.AlignEnd)
		frac := 0.0
		var parts []string
		if st.Steps > 0 {
			frac = float64(st.Step-1) / float64(st.Steps)
			parts = append(parts, fmt.Sprintf("step %d of %d", st.Step, st.Steps))
		}
		if st.Target != 0 {
			f := math.Max(0, math.Min(1, st.Achieved/st.Target))
			if st.Steps > 0 {
				frac += f / float64(st.Steps)
			} else {
				frac = f
			}
			if st.Unit == "deg" {
				parts = append(parts, fmt.Sprintf("%.0f° / %.0f°", st.Achieved, st.Target))
			} else {
				parts = append(parts, fmt.Sprintf("%.2f / %.2f m", st.Achieved, st.Target))
			}
		}
		detail := strings.Join(parts, " · ")
		h.fill(screen, x+22, y+48, pw-44, 8, withAlpha(colText, 24))
		h.fill(screen, x+22, y+48, (pw-44)*math.Max(0, math.Min(1, frac)), 8, colAgent)
		h.text(screen, strings.TrimSpace(detail+"    X stops"), 14, false, x+22, y+70, colDim, text.AlignStart)
		return true
	case st.Last != nil:
		h.panel(screen, x, y, pw, 48, colPanel)
		msg, c := "Done", colGood
		if !st.Last.Completed {
			msg, c = "Stopped: "+st.Last.StoppedBy, colWarn
		}
		h.text(screen, fmt.Sprintf("%s · %s", strings.ToUpper(st.Last.Kind), msg), 16, true, x+22, y+24, c, text.AlignStart)
		return true
	}
	return false
}

// ---------- Speed gauge ----------

func (h *hud) drawSpeedGauge(screen *ebiten.Image, cx, cy, r float64, speed, max float64, active bool) {
	a0, a1 := math.Pi*0.75, math.Pi*2.25
	h.arc(screen, cx, cy, r, a0, a1, 7, withAlpha(colText, 26))
	f := math.Max(0, math.Min(1, speed/max))
	if f > 0.01 {
		c := colAccent
		if !active {
			c = withAlpha(colAccent, 120)
		}
		h.arc(screen, cx, cy, r, a0, a0+(a1-a0)*f, 7, c)
	}
	for i := 0; i <= 4; i++ {
		a := a0 + (a1-a0)*float64(i)/4
		h.line(screen, cx+math.Cos(a)*(r-12), cy+math.Sin(a)*(r-12), cx+math.Cos(a)*(r-6), cy+math.Sin(a)*(r-6), 2, withAlpha(colText, 90))
	}
	h.mono(screen, fmt.Sprintf("%.2f", speed), 24, cx, cy-4, colText, text.AlignCenter)
	h.text(screen, "m/s", 12, false, cx, cy+18, colDim, text.AlignCenter)
}

// ---------- Status chips in the top bar ----------

func (h *hud) chip(screen *ebiten.Image, x float64, label string, c color.NRGBA) float64 {
	lw := h.width(label, 15, true) + 24
	h.panel(screen, x, 19, lw, 28, withAlpha(c, 38))
	h.text(screen, label, 15, true, x+12, 33, c, text.AlignStart)
	return x + lw + 10
}

// fmtRange shows distances in 5 cm steps so the readout does not jitter.
func fmtRange(m float64) string {
	return fmt.Sprintf("%.2f m", math.Round(m*20)/20)
}

package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/shell-company/scout/internal/control"
	"github.com/shell-company/scout/internal/manual"
)

func readInput() manual.Frame {
	held := ebiten.IsKeyPressed
	f := manual.Frame{Focused: ebiten.IsFocused(), Keyboard: manual.Keyboard{
		Forward: held(ebiten.KeyW) || held(ebiten.KeyArrowUp), Back: held(ebiten.KeyS) || held(ebiten.KeyArrowDown),
		Left: held(ebiten.KeyA) || held(ebiten.KeyArrowLeft), Right: held(ebiten.KeyD) || held(ebiten.KeyArrowRight),
		StrafeLeft: held(ebiten.KeyQ), StrafeRight: held(ebiten.KeyE),
	}}
	for _, b := range []struct {
		key    ebiten.Key
		button manual.Buttons
	}{
		{ebiten.KeyX, manual.Stop}, {ebiten.KeyEscape, manual.Exit}, {ebiten.KeyP, manual.Faster}, {ebiten.KeyO, manual.Slower},
		{ebiten.KeyH, manual.Dock}, {ebiten.KeySpace, manual.Screenshot}, {ebiten.Key9, manual.Dim}, {ebiten.Key0, manual.Bright},
	} {
		if held(b.key) {
			f.Keyboard.Buttons |= b.button
		}
	}
	for _, id := range ebiten.AppendGamepadIDs(nil) {
		p := manual.Pad{ID: int(id), Name: ebiten.GamepadName(id), Standard: ebiten.IsStandardGamepadLayoutAvailable(id)}
		if p.Standard {
			p.LeftX = ebiten.StandardGamepadAxisValue(id, ebiten.StandardGamepadAxisLeftStickHorizontal)
			p.LeftY = ebiten.StandardGamepadAxisValue(id, ebiten.StandardGamepadAxisLeftStickVertical)
			p.RightX = ebiten.StandardGamepadAxisValue(id, ebiten.StandardGamepadAxisRightStickHorizontal)
			for _, b := range []struct {
				button ebiten.StandardGamepadButton
				action manual.Buttons
			}{
				{ebiten.StandardGamepadButtonRightBottom, manual.Screenshot}, // Xbox A
				{ebiten.StandardGamepadButtonRightRight, manual.Stop},        // Xbox B
				{ebiten.StandardGamepadButtonRightLeft, manual.AutoLight},    // Xbox X
				{ebiten.StandardGamepadButtonRightTop, manual.Dock},          // Xbox Y
				{ebiten.StandardGamepadButtonFrontTopLeft, manual.Slower},
				{ebiten.StandardGamepadButtonFrontTopRight, manual.Faster},
				{ebiten.StandardGamepadButtonLeftTop, manual.Bright},
				{ebiten.StandardGamepadButtonLeftBottom, manual.Dim},
				{ebiten.StandardGamepadButtonCenterRight, manual.Exit}, // Xbox Menu/Start
			} {
				if ebiten.IsStandardGamepadButtonPressed(id, b.button) {
					p.Buttons |= b.action
				}
			}
			if p.Buttons&(manual.Faster|manual.Slower) == manual.Faster|manual.Slower {
				p.Buttons |= manual.Stop
			}
		}
		f.Pads = append(f.Pads, p)
	}
	return f
}

type Game struct {
	app         *application
	input       manual.Mapper
	inputStatus manual.Result
	inputTest   bool
	lastStop    bool
	unfocused   bool
	showHelp    bool
	host        textField // connect screen address
	menu        robotMenu
	finder      finder
	// holdInput keeps driving input off until keys used by an overlay are
	// released, so Esc or Enter cannot leak into the controller.
	holdInput bool
	hud       *hud
	cleanHUD  bool // Tab hides the map, heading tape and event feed
}

// finder scans the local network for robots from the connect screen.
type finder struct {
	mu       sync.Mutex
	scanning bool
	found    []string
	sel      int
}

func (f *finder) start(ctx context.Context) {
	f.mu.Lock()
	if f.scanning {
		f.mu.Unlock()
		return
	}
	f.scanning, f.found, f.sel = true, nil, -1
	f.mu.Unlock()
	go func() {
		found := discoverRobots(ctx)
		f.mu.Lock()
		f.scanning, f.found = false, found
		f.mu.Unlock()
	}()
}

func (f *finder) snapshot() (bool, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scanning, append([]string(nil), f.found...), f.sel
}

func newGame(a *application, inputTest bool) *Game {
	_, _, t := a.connState()
	return &Game{app: a, input: manual.Mapper{KeyboardOnly: *flagControlScheme == "keyboard"}, inputTest: inputTest,
		host: textField{text: t.Host, max: 253, allow: hostRune}, hud: newHUD()}
}

func (g *Game) Update() error {
	if g.app.ctx.Err() != nil {
		return ebiten.Termination
	}
	// Plain keys: Mac laptops need Fn for F1-F12.
	if inpututil.IsKeyJustPressed(ebiten.KeySlash) && g.menu.page == menuClosed {
		g.showHelp = !g.showHelp
	}
	if !g.inputTest && !g.app.ready.Load() {
		g.menu.page = menuClosed
		return g.updateConnect()
	}
	if !g.inputTest && g.menu.page == menuClosed && inpututil.IsKeyJustPressed(ebiten.KeyM) {
		g.menu.open(g)
		g.showHelp = false
		return nil
	}
	if g.menu.page == menuClosed {
		switch {
		case inpututil.IsKeyJustPressed(ebiten.KeyTab):
			g.cleanHUD = !g.cleanHUD
		case inpututil.IsKeyJustPressed(ebiten.KeyF):
			ebiten.SetFullscreen(!ebiten.IsFullscreen())
		case g.inputTest:
		case inpututil.IsKeyJustPressed(ebiten.KeyV):
			g.app.toggleVoice()
		case inpututil.IsKeyJustPressed(ebiten.KeyR):
			g.app.toggleRecording()
		case inpututil.IsKeyJustPressed(ebiten.KeyG):
			gs := g.app.guardSettings()
			gs.Armed = !gs.Armed
			if err := g.app.setGuard(gs); err != nil {
				g.app.notices.add(err.Error(), noticeBad)
			} else if gs.Armed {
				g.app.notices.add("Guard mode armed: alerts for motion"+map[bool]string{true: ", people and pets", false: ""}[gs.Objects], noticeGood)
			} else {
				g.app.notices.add("Guard mode off", noticeInfo)
			}
		}
	}
	if g.menu.update(g) {
		g.holdInput = true
		return nil
	}
	if g.holdInput {
		if ebiten.IsKeyPressed(ebiten.KeyEscape) || ebiten.IsKeyPressed(ebiten.KeyEnter) || ebiten.IsKeyPressed(ebiten.KeyM) || ebiten.IsKeyPressed(ebiten.KeyY) {
			return nil
		}
		g.holdInput = false
	}
	result := g.applyInput(readInput())
	g.inputStatus = result
	if result.Quit {
		g.app.cancel()
		return ebiten.Termination
	}
	for _, command := range result.Actions {
		if g.inputTest {
			log.Printf("Input test action: %s (not sent to robot)", command)
			continue
		}
		err := g.app.actions.Submit(func(ctx context.Context) error {
			switch command {
			case "screenshot":
				name, err := saveScreenshot()
				if err == nil {
					g.app.notices.add("Screenshot saved: "+name, noticeGood)
				}
				return err
			case "dim":
				return g.app.action(ctx, "light", 0)
			case "bright":
				return g.app.action(ctx, "light", 1)
			case "auto-light":
				return g.app.action(ctx, "light", 4)
			default:
				if err := g.app.action(ctx, "home", 0); err != nil {
					return err
				}
				g.app.notices.add("Docking started - press X to cancel", noticeInfo)
				return nil
			}
		})
		if err != nil {
			log.Print(err)
			g.app.notices.add(err.Error(), noticeBad)
		}
	}
	return nil
}

func (g *Game) cancelDock() {
	if !g.app.dockingActive() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := g.app.stopAll(ctx); err != nil {
			log.Printf("stop failed: %v", err)
		}
	}()
}

// Losing focus revokes human input once; an unfocused window does not hold an
// artificial stop button forever and prevent explicitly requested agent control.
func (g *Game) applyInput(frame manual.Frame) manual.Result {
	result := g.input.Update(frame)
	wasUnfocused := g.unfocused
	g.unfocused = !frame.Focused
	if result.Stop && (!g.lastStop || frame.Focused && wasUnfocused) {
		g.cancelDock()
	}
	g.lastStop = result.Stop
	stop := result.Stop
	if !frame.Focused && wasUnfocused {
		stop = false
	}
	motion := result.Motion
	if !frame.Focused {
		motion = control.Motion{}
	}
	g.app.control.Manual(motion, stop, time.Now())
	return result
}

// updateConnect edits the robot address until a connection succeeds.
func (g *Game) updateConnect() error {
	phase, _, _ := g.app.connState()
	if s := g.app.takeSuggestion(); s != "" {
		g.host.text = s
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
		g.app.cancel()
		return ebiten.Termination
	}
	if phase == connConnecting {
		return nil
	}
	_, found, sel := g.finder.snapshot()
	switch {
	case pressed(ebiten.KeyTab):
		g.finder.start(g.app.ctx)
	case len(found) > 0 && pressed(ebiten.KeyArrowDown, ebiten.KeyArrowUp):
		step := 1
		if pressed(ebiten.KeyArrowUp) {
			step = len(found) - 1
		}
		sel = (sel + step + len(found)) % len(found)
		if sel < 0 {
			sel = 0
		}
		g.finder.mu.Lock()
		g.finder.sel = sel
		g.finder.mu.Unlock()
		g.host.text = found[sel]
	case pressed(ebiten.KeyEnter, ebiten.KeyNumpadEnter):
		host := strings.TrimSpace(g.host.text)
		if err := validHost(host); err != nil {
			g.app.setConn(connFailed, err.Error())
			return nil
		}
		set := false
		flagVisitSSH(&set)
		g.app.startConnect(target{}.withHost(host, g.app.settings.SSHUser, set))
	default:
		g.host.update()
	}
	return nil
}

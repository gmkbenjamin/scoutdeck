package main

import (
	"context"
	"log"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
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

func (g *Game) Update() error {
	if g.app.ctx.Err() != nil {
		return ebiten.Termination
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
				return saveScreenshot()
			case "dim":
				return g.app.action(ctx, "light", 0)
			case "bright":
				return g.app.action(ctx, "light", 1)
			case "auto-light":
				return g.app.action(ctx, "light", 4)
			default:
				return g.app.action(ctx, "home", 0)
			}
		})
		if err != nil {
			log.Print(err)
		}
	}
	return nil
}

func (g *Game) cancelDock() {
	if g.app.dock == nil || !g.app.dock.Active() {
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

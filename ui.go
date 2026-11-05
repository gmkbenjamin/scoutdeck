package main

import (
	"context"
	"log"
	"math"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/shell-company/scout/internal/control"
)

func (g *Game) pressed(key ebiten.Key) bool {
	down := ebiten.IsKeyPressed(key)
	was := g.keys[key]
	g.keys[key] = down
	return down && !was
}

func (g *Game) Update() error {
	if g.app.ctx.Err() != nil {
		return ebiten.Termination
	}
	if ebiten.IsKeyPressed(ebiten.KeyEscape) {
		g.app.cancel()
		return ebiten.Termination
	}
	if !ebiten.IsFocused() {
		g.app.control.Manual(control.Motion{}, g.wasFocused, time.Now())
		g.wasFocused = false
		return nil
	}
	g.wasFocused = true
	m := control.Motion{}
	stop := ebiten.IsKeyPressed(ebiten.KeyX)
	faster, slower := g.pressed(ebiten.KeyP), g.pressed(ebiten.KeyO)
	if *flagControlScheme == "joystick" {
		ids := ebiten.AppendGamepadIDs(nil)
		if len(ids) == 0 || !ebiten.IsStandardGamepadLayoutAvailable(ids[0]) {
			if g.hadJoystick {
				log.Print("Joystick disconnected; stopping")
				g.cancelDock()
			}
			g.app.control.Manual(m, g.hadJoystick, time.Now())
			g.hadJoystick = false
			return nil
		}
		g.hadJoystick = true
		id := ids[0]
		if ebiten.IsStandardGamepadButtonPressed(id, ebiten.StandardGamepadButtonCenterRight) {
			g.app.cancel()
			return ebiten.Termination
		}
		var bumpers uint8
		if ebiten.IsStandardGamepadButtonPressed(id, ebiten.StandardGamepadButtonFrontTopLeft) {
			bumpers |= 1
		}
		if ebiten.IsStandardGamepadButtonPressed(id, ebiten.StandardGamepadButtonFrontTopRight) {
			bumpers |= 2
		}
		stop = stop || bumpers == 3
		if !stop {
			slower = slower || bumpers == 1 && g.lastBumpers != 1
			faster = faster || bumpers == 2 && g.lastBumpers != 2
		}
		g.lastBumpers = bumpers
		g.adjustSpeed(faster, slower)
		axis := func(ax ebiten.StandardGamepadAxis) float64 {
			v := ebiten.StandardGamepadAxisValue(id, ax)
			if math.Abs(v) < .015 {
				return 0
			}
			return v * .5
		}
		m = control.JoystickMotion(axis(ebiten.StandardGamepadAxisLeftStickHorizontal), -axis(ebiten.StandardGamepadAxisLeftStickVertical), axis(ebiten.StandardGamepadAxisRightStickHorizontal), g.speed, stop)
	} else {
		g.adjustSpeed(faster, slower)
		if ebiten.IsKeyPressed(ebiten.KeyW) {
			m.Y = g.speed
		} else if ebiten.IsKeyPressed(ebiten.KeyS) {
			m.Y = -g.speed
		}
		if ebiten.IsKeyPressed(ebiten.KeyQ) {
			m.X = -.2
		} else if ebiten.IsKeyPressed(ebiten.KeyE) {
			m.X = .2
		}
		if ebiten.IsKeyPressed(ebiten.KeyA) {
			m.Z = 1.8
		} else if ebiten.IsKeyPressed(ebiten.KeyD) {
			m.Z = -1.8
		}
	}
	if stop && !g.lastStop {
		g.cancelDock()
	}
	g.lastStop = stop
	g.app.control.Manual(m, stop, time.Now())
	for key, command := range map[ebiten.Key]string{ebiten.KeyH: "home", ebiten.Key9: "dim", ebiten.Key0: "bright", ebiten.KeySpace: "screenshot"} {
		if !g.pressed(key) {
			continue
		}
		if command == "home" {
			g.app.control.Stop()
		}
		err := g.app.actions.Submit(func(ctx context.Context) error {
			switch command {
			case "screenshot":
				return saveScreenshot()
			case "dim":
				return g.app.action(ctx, "light", 0)
			case "bright":
				return g.app.action(ctx, "light", 1)
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

func (g *Game) adjustSpeed(faster, slower bool) {
	if faster != slower {
		if faster {
			g.speed += .1
		} else {
			g.speed -= .1
		}
	}
	g.speed = math.Max(.1, math.Min(1, g.speed))
}

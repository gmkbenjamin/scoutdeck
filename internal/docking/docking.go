// Package docking tracks a single native docking operation and verifies power independently.
package docking

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Power struct {
	Status     string    `json:"status"`
	Connected  bool      `json:"external_power"`
	ObservedAt time.Time `json:"observed_at"`
}

type Operation struct {
	ID               string    `json:"id"`
	State            string    `json:"state"`
	Active           bool      `json:"active"`
	ChargingVerified bool      `json:"charging_verified"`
	FirmwarePhase    int32     `json:"firmware_phase"`
	StartedAt        time.Time `json:"started_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Deadline         time.Time `json:"deadline"`
	Power            Power     `json:"power"`
	Error            string    `json:"error,omitempty"`
}

type Hooks struct {
	// Prepare stops manual/agent control and flushes a zero command.
	Prepare func(context.Context) error
	Start   func(context.Context) error
	Cancel  func(context.Context) error
	Probe   func(context.Context) (Power, error)
}

type Manager struct {
	mu       sync.Mutex
	cancelMu sync.Mutex
	parent   context.Context
	hooks    Hooks
	op       Operation
	cancel   context.CancelFunc
	done     chan struct{}
	phaseAt  time.Time
	interval time.Duration
}

func New(ctx context.Context, hooks Hooks) *Manager {
	return &Manager{parent: ctx, hooks: hooks, op: Operation{State: "idle"}, interval: time.Second}
}
func (m *Manager) Snapshot() Operation { m.mu.Lock(); defer m.mu.Unlock(); return m.op }
func (m *Manager) Active() bool        { return m.Snapshot().Active }

func (m *Manager) Phase(phase int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.op.Active || m.op.State == "cancel_failed" {
		return
	}
	m.op.FirmwarePhase = phase
	m.phaseAt = time.Now()
	m.op.UpdatedAt = m.phaseAt
	if name, ok := map[int32]string{1: "searching", 2: "aligning", 3: "reversing", 4: "verifying", 8: "searching"}[phase]; ok {
		m.op.State = name
	}
}

// WhileIdle serializes drive acceptance with the start of native docking.
func (m *Manager) WhileIdle(fn func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.op.Active {
		return fmt.Errorf("docking owns movement; cancel docking before driving")
	}
	return fn()
}

func (m *Manager) Start(timeout time.Duration) (Operation, error) {
	if timeout < 10*time.Second || timeout > 180*time.Second {
		return Operation{}, fmt.Errorf("timeout_seconds must be 10..180")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.parent.Err() != nil {
		return Operation{}, fmt.Errorf("controller is shutting down")
	}
	if m.op.Active {
		return m.op, fmt.Errorf("docking already active; poll or cancel it")
	}
	if m.hooks.Probe == nil {
		return m.op, fmt.Errorf("docking requires -ssh for live charging verification")
	}
	now := time.Now()
	ctx, cancel := context.WithTimeout(m.parent, timeout)
	m.cancel = cancel
	m.done = make(chan struct{})
	m.phaseAt = time.Time{}
	m.op = Operation{ID: fmt.Sprintf("dock-%d", now.UnixNano()), State: "starting", Active: true, StartedAt: now, UpdatedAt: now, Deadline: now.Add(timeout)}
	op := m.op
	go m.run(ctx, m.done)
	return op, nil
}

func (m *Manager) Cancel(ctx context.Context) error {
	m.cancelMu.Lock()
	defer m.cancelMu.Unlock()
	m.mu.Lock()
	if !m.op.Active {
		m.mu.Unlock()
		return nil
	}
	if m.op.State == "cancel_failed" {
		m.mu.Unlock()
		err := m.hooks.Cancel(ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			m.op.Error = err.Error()
			return err
		}
		m.op.Active = false
		m.op.State = "cancelled"
		m.op.Error = ""
		m.op.UpdatedAt = time.Now()
		return nil
	}
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		op := m.Snapshot()
		if op.Active {
			return fmt.Errorf("could not confirm docking cancellation: %s", op.Error)
		}
		return nil
	}
}

func (m *Manager) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	state, detail := "failed", ""
	defer func() {
		// Cancellation has its own deadline, including when the operation timed out.
		cleanup, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		err := m.hooks.Cancel(cleanup)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.op.State = state
		m.op.Error = detail
		m.op.Active = false
		m.op.UpdatedAt = time.Now()
		if err != nil {
			m.op.State = "cancel_failed"
			m.op.Error = fmt.Sprintf("%s; cancellation: %v", detail, err)
			m.op.Active = true
		}
		m.cancel()
	}()
	finishContext := func() {
		state = "cancelled"
		if ctx.Err() == context.DeadlineExceeded {
			state = "timed_out"
		}
		detail = ctx.Err().Error()
	}
	if ctx.Err() != nil {
		finishContext()
		return
	}
	if err := m.hooks.Prepare(ctx); err != nil {
		detail = err.Error()
		return
	}
	// Establish a fresh baseline before the robot is allowed to start searching.
	p, err := m.hooks.Probe(ctx)
	if err != nil {
		detail = "cannot verify initial charging state: " + err.Error()
		return
	}
	m.setPower(p)
	started := false
	connectedCount := 0
	if !p.Connected {
		if ctx.Err() != nil {
			finishContext()
			return
		}
		if err := m.startNative(ctx); err != nil {
			detail = "docking service failed: " + err.Error()
			return
		}
		started = true
	} else {
		connectedCount = 1
	}
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			finishContext()
			return
		case <-tick.C:
			p, err = m.hooks.Probe(ctx)
			if err != nil {
				connectedCount = 0
				detail = "charging check failed: " + err.Error()
				continue
			}
			m.setPower(p)
			if p.Connected {
				connectedCount++
			} else {
				connectedCount = 0
			}
			if connectedCount >= 2 {
				m.mu.Lock()
				m.op.ChargingVerified = true
				m.mu.Unlock()
				state = "charging"
				detail = ""
				return
			}
			if !started {
				if err = m.startNative(ctx); err != nil {
					detail = err.Error()
					return
				}
				started = true
			}
			m.mu.Lock()
			phase := m.op.FirmwarePhase
			phaseAt := m.phaseAt
			m.mu.Unlock()
			if phase == 5 && !phaseAt.IsZero() {
				detail = "robot reported docking failure without external power"
				return
			}
		}
	}
}

func (m *Manager) startNative(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	return m.hooks.Start(ctx)
}

func (m *Manager) setPower(p Power) {
	m.mu.Lock()
	m.op.Power = p
	m.op.UpdatedAt = time.Now()
	m.mu.Unlock()
}

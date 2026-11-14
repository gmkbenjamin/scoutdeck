// Package docking tracks a single native docking operation and verifies power independently.
package docking

import (
	"context"
	"fmt"
	"log"
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
	// ShouldApproach reports synchronously during Start whether Approach should run first.
	ShouldApproach func() bool
	// Approach optionally drives near the dock before native docking takes over.
	Approach func(context.Context) error
	Start    func(context.Context) error
	Cancel   func(context.Context) error
	Probe    func(context.Context) (Power, error)
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
	// firmwareActive is set once the firmware reports a working phase in this
	// operation, so an idle report left over from before the start is ignored.
	firmwareActive bool
	interval       time.Duration
}

// Firmware phases on /CoreNode/going_home_status (BACK_UP_* constants).
const (
	phaseSuccess  = 4
	phaseFail     = 5
	phaseInactive = 6
	phaseCancel   = 7
)

// MaxTimeout bounds one docking operation. A search from across a room can take
// two minutes and the firmware retries missed reversals itself, so 3 minutes
// was too short in practice.
const MaxTimeout = 600 * time.Second

func New(ctx context.Context, hooks Hooks) *Manager {
	return &Manager{parent: ctx, hooks: hooks, op: Operation{State: "idle"}, interval: time.Second}
}
func (m *Manager) Snapshot() Operation { m.mu.Lock(); defer m.mu.Unlock(); return m.op }
func (m *Manager) Active() bool        { return m.Snapshot().Active }

// NativeActive reports whether the robot firmware is actively docking (after any approach step).
func (m *Manager) NativeActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.op.Active && m.op.State != "approaching"
}

func (m *Manager) Phase(phase int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.op.Active || m.op.State == "approaching" || m.op.State == "cancel_failed" {
		return
	}
	m.op.FirmwarePhase = phase
	m.phaseAt = time.Now()
	m.op.UpdatedAt = m.phaseAt
	if name, ok := map[int32]string{1: "searching", 2: "aligning", 3: "reversing", phaseSuccess: "verifying", 8: "searching"}[phase]; ok {
		m.op.State = name
		m.firmwareActive = true
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

// WhileApproaching serializes pre-docking approach steps so external drive
// requests stay blocked while only the docking approach moves the robot.
func (m *Manager) WhileApproaching(fn func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.op.Active || m.op.State != "approaching" {
		return fmt.Errorf("docking is not in the approach phase")
	}
	return fn()
}

func (m *Manager) Start(timeout time.Duration) (Operation, error) {
	if timeout < 10*time.Second || timeout > MaxTimeout {
		return Operation{}, fmt.Errorf("timeout_seconds must be 10..%d", int(MaxTimeout/time.Second))
	}
	approaching := m.hooks.Approach != nil && m.hooks.ShouldApproach != nil && m.hooks.ShouldApproach()
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
	m.firmwareActive = false
	state := "starting"
	if approaching {
		state = "approaching"
	}
	m.op = Operation{ID: fmt.Sprintf("dock-%d", now.UnixNano()), State: state, Active: true, StartedAt: now, UpdatedAt: now, Deadline: now.Add(timeout)}
	op := m.op
	go m.run(ctx, m.done, approaching)
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

func (m *Manager) run(ctx context.Context, done chan struct{}, approaching bool) {
	defer close(done)
	state, detail := "failed", ""
	nativeStarted := false
	defer func() {
		var err error
		if nativeStarted {
			// Cancellation has its own deadline, including when the operation timed out.
			cleanup, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			err = m.hooks.Cancel(cleanup)
			cancel()
		}
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
		// Keyboard and gamepad docking has no API caller to report to.
		if m.op.Error != "" {
			log.Printf("docking %s: %s", m.op.State, m.op.Error)
		} else {
			log.Printf("docking %s", m.op.State)
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
	failOrContext := func(prefix string, err error) {
		if ctx.Err() != nil {
			finishContext()
			return
		}
		detail = prefix + err.Error()
	}
	launchNative := func() bool {
		if ctx.Err() != nil {
			finishContext()
			return false
		}
		nativeStarted = true
		if err := m.startNative(ctx); err != nil {
			failOrContext("docking service failed: ", err)
			return false
		}
		return true
	}
	if ctx.Err() != nil {
		finishContext()
		return
	}
	if approaching {
		if err := m.hooks.Approach(ctx); err != nil {
			failOrContext("", err)
			return
		}
		m.mu.Lock()
		if m.op.Active && m.op.State == "approaching" {
			m.op.State = "starting"
			m.op.UpdatedAt = time.Now()
		}
		m.mu.Unlock()
	}
	if err := m.hooks.Prepare(ctx); err != nil {
		failOrContext("", err)
		return
	}
	// Establish a fresh baseline before the robot is allowed to start searching.
	p, err := m.hooks.Probe(ctx)
	if err != nil {
		failOrContext("cannot verify initial charging state: ", err)
		return
	}
	m.setPower(p)
	started := false
	connectedCount := 0
	if !p.Connected {
		if !launchNative() {
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
				if !launchNative() {
					return
				}
				started = true
			}
			m.mu.Lock()
			phase := m.op.FirmwarePhase
			phaseAt := m.phaseAt
			active := m.firmwareActive
			m.mu.Unlock()
			// The firmware stopped without contact. Allow a few probes first: it
			// can leave its routine just before the charging status flips.
			stopped := phase == phaseFail || active && (phase == phaseInactive || phase == phaseCancel)
			if stopped && !phaseAt.IsZero() && connectedCount == 0 && time.Since(phaseAt) >= 5*m.interval {
				detail = map[int32]string{
					phaseFail:     "robot reported docking failure without external power",
					phaseInactive: "robot stopped docking without external power",
					phaseCancel:   "robot cancelled docking without external power",
				}[phase]
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

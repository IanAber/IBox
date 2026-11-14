package main

// SOC-based load control. Replaces the "SOC Load Control" tab:
// Battery SOC Input -> Read 8 Relay States -> Save Actual Relay States ->
// Evaluate SOC / Relays -> Write Relay + status to dashboard.

import (
	"context"
	"log"
	"sync"
	"time"
)

const relayRepeatDelay = 5 * time.Second       // re-send an unconfirmed write after this
const relayCommandGap = 100 * time.Millisecond // relay client commandDelay

type pendingCmd struct {
	state bool
	time  time.Time
}

type CircuitStatus struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	StartSOC float64 `json:"startSOC"`
	StopSOC  float64 `json:"stopSOC"`
	Enabled  bool    `json:"enabled"`
	Desired  bool    `json:"desiredState"`
}

// DashboardStatus = output of "Merge Live Status for Dashboard".
type DashboardStatus struct {
	Circuits            []CircuitStatus `json:"circuits"`
	CurrentSOC          float64         `json:"currentSOC"`
	RelayActionsEnabled bool            `json:"relayActionsEnabled"`
	RelayHWSimulated    bool            `json:"relayHwSimulated"`
}

type SimState struct {
	ReadEnabled         bool    `json:"readEnabled"`
	RelayActionsEnabled bool    `json:"relayActionsEnabled"`
	RelayHWSimulated    bool    `json:"relayHwSimulated"`
	Data                EG4Data `json:"data"`
}

type App struct {
	mu sync.Mutex

	data     EG4Data
	haveData bool // false until real or simulated telemetry has arrived

	readEnabled         bool // global.eg4_read_enabled
	relayActionsEnabled bool // global.eg4_relay_actions_enabled
	relayHWSimulated    bool // global.eg4_relay_hw_simulated

	cfg         CircuitConfig
	relayStates [8]bool
	pending     map[int]pendingCmd
	lastStatus  *DashboardStatus

	configPath string
	relays     *RelayModule
	hub        *Hub
	evalCh     chan struct{}
}

func NewApp(configPath string, relays *RelayModule, hub *Hub, hwSimulated bool) *App {
	return &App{
		data:                defaultEG4Data(),
		readEnabled:         true,
		relayActionsEnabled: true,
		relayHWSimulated:    hwSimulated,
		cfg:                 defaultConfig(),
		pending:             map[int]pendingCmd{},
		configPath:          configPath,
		relays:              relays,
		hub:                 hub,
		evalCh:              make(chan struct{}, 1),
	}
}

// TriggerEvaluate queues a load-control pass. Bursts coalesce into one.
func (a *App) TriggerEvaluate() {
	select {
	case a.evalCh <- struct{}{}:
	default:
	}
}

// RunController serialises every evaluation on one goroutine, which is how
// Node-RED's single-threaded runtime behaved.
func (a *App) RunController(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.evalCh:
			a.evaluateOnce()
		}
	}
}

// desiredState applies the hysteresis rule.
func desiredState(soc, start, stop float64, current bool) bool {
	if soc >= start {
		return true
	}
	if soc <= stop {
		return false
	}
	return current
}

type relayWrite struct {
	addr  int
	state bool
	name  string
}

func (a *App) evaluateOnce() bool {
	a.mu.Lock()
	if !a.haveData {
		a.mu.Unlock()
		log.Printf("control: waiting for SOC (no live or simulated data yet)")
		return false
	}
	soc := a.data.BatterySOCPct
	hwSim := a.relayHWSimulated
	a.mu.Unlock()

	// "Battery SOC Input" range check
	if soc < 0 || soc > 100 {
		log.Printf("control: invalid battery SOC %v", soc)
		return false
	}

	// Real hardware: read actual coil states first, clear confirmed commands.
	if !hwSim {
		states, err := a.relays.ReadStates()
		if err != nil {
			log.Printf("control: relay read failed: %v (skipping this pass)", err)
			return false
		}
		a.mu.Lock()
		a.relayStates = states
		for i, p := range a.pending {
			if p.state == states[i] {
				delete(a.pending, i)
			}
		}
		a.mu.Unlock()
	}

	a.mu.Lock()
	// Re-read flags: they may have been toggled while the relay read ran.
	hwSim = a.relayHWSimulated
	actions := a.relayActionsEnabled
	now := time.Now()
	var writes []relayWrite
	status := &DashboardStatus{CurrentSOC: soc, RelayActionsEnabled: actions, RelayHWSimulated: hwSim}

	for i, c := range a.cfg.Circuits {
		if i >= 8 {
			break
		}
		current := a.relayStates[i]
		want := desiredState(soc, c.StartSOC, c.StopSOC, current)
		reported := current

		if want != current && actions {
			if hwSim {
				// Software model: apply immediately.
				a.relayStates[i] = want
				reported = want
				delete(a.pending, i)
			} else {
				prev, ok := a.pending[i]
				if !ok || prev.state != want || now.Sub(prev.time) >= relayRepeatDelay {
					writes = append(writes, relayWrite{addr: i, state: want, name: c.Name})
					a.pending[i] = pendingCmd{state: want, time: now}
				}
			}
		}
		status.Circuits = append(status.Circuits, CircuitStatus{
			ID: c.ID, Name: c.Name, StartSOC: c.StartSOC, StopSOC: c.StopSOC,
			Enabled: reported, Desired: want,
		})
	}
	a.lastStatus = status
	a.mu.Unlock()

	for _, w := range writes {
		if err := a.relays.WriteCoil(w.addr, w.state); err != nil {
			log.Printf("control: write relay %d (%s) -> %v failed: %v", w.addr+1, w.name, onOff(w.state), err)
		} else {
			log.Printf("control: relay %d (%s) -> %s at SOC %.0f%%", w.addr+1, w.name, onOff(w.state), soc)
		}
		time.Sleep(relayCommandGap)
	}

	a.hub.Broadcast("status", status)
	return true
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// ---- Simulator / mode toggles ("EG4 Test Simulator" tab) ----

func (a *App) simStateLocked() SimState {
	return SimState{
		ReadEnabled:         a.readEnabled,
		RelayActionsEnabled: a.relayActionsEnabled,
		RelayHWSimulated:    a.relayHWSimulated,
		Data:                a.data,
	}
}

func (a *App) SimState() SimState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.simStateLocked()
}

// SetReadEnabled = "Set Read Enabled". Entering test mode turns relay
// actions off as a safety measure; resuming live read turns them back on.
func (a *App) SetReadEnabled(v bool) {
	a.mu.Lock()
	a.readEnabled = v
	a.relayActionsEnabled = v
	s := a.simStateLocked()
	a.mu.Unlock()
	log.Printf("mode: live EG4 read %s, relay actions %s", onOff(v), onOff(v))
	a.hub.Broadcast("simstate", s)
}

// SetRelayActionsEnabled = "Set Relay Actions Enabled".
func (a *App) SetRelayActionsEnabled(v bool) {
	a.mu.Lock()
	a.relayActionsEnabled = v
	s := a.simStateLocked()
	a.mu.Unlock()
	log.Printf("mode: relay actions %s", onOff(v))
	a.hub.Broadcast("simstate", s)
	if v {
		a.TriggerEvaluate()
	}
}

// SetRelayHWSimulated = "Set Relay HW Simulated".
func (a *App) SetRelayHWSimulated(v bool) {
	a.mu.Lock()
	a.relayHWSimulated = v
	s := a.simStateLocked()
	a.mu.Unlock()
	log.Printf("mode: simulate relay hardware %s", onOff(v))
	a.hub.Broadcast("simstate", s)
}

// StoreSimulatedData = "Store Simulated EG4 Data". Ignored while live read
// is enabled so test values can't stomp on real telemetry.
func (a *App) StoreSimulatedData(d EG4Data) bool {
	a.mu.Lock()
	if a.readEnabled {
		a.mu.Unlock()
		return false
	}
	d.LastUpdated = time.Now().UTC().Format(time.RFC3339Nano)
	a.data = d
	a.haveData = true
	a.mu.Unlock()
	a.hub.Broadcast("data", d)
	a.TriggerEvaluate()
	return true
}

package main

// Communication health for the EG4 inverter and the relay module, shown as
// warning/error banners on the configuration page.

import (
	"context"
	"time"
)

// eg4StalePolls is how many poll intervals may pass without a successful
// read before EG4 data is reported as stale.
const eg4StalePolls = 3

const healthCheckEvery = 1 * time.Second

const (
	healthOK      = "ok"
	healthWarning = "warning"
	healthError   = "error"
)

type CommStatus struct {
	State   string `json:"state"` // ok | warning | error
	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"` // underlying error text
	LastOK  string `json:"lastOk,omitempty"` // RFC3339; empty if never
}

type CommHealth struct {
	EG4    CommStatus `json:"eg4"`
	Relays CommStatus `json:"relays"`
}

// commState is the raw record of one link's most recent outcome.
type commState struct {
	lastOK time.Time
	err    string // last error; empty after a success
}

func (c *commState) record(now time.Time, err error) {
	if err != nil {
		c.err = err.Error()
		return
	}
	c.lastOK = now
	c.err = ""
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (a *App) recordEG4(err error) {
	a.mu.Lock()
	a.eg4Comm.record(a.now(), err)
	a.mu.Unlock()
}

func (a *App) recordRelay(err error) {
	a.mu.Lock()
	a.relayComm.record(a.now(), err)
	a.mu.Unlock()
}

func (a *App) healthLocked(now time.Time) CommHealth {
	var h CommHealth

	e := a.eg4Comm
	h.EG4.LastOK = fmtTime(e.lastOK)
	stale := time.Duration(eg4StalePolls) * a.pollEvery
	switch {
	case !a.eg4Polling:
		h.EG4 = CommStatus{State: healthWarning,
			Message: "EG4 polling is disabled (-no-eg4). Circuits are controlled only by test simulator data."}
	case e.err != "":
		h.EG4.State, h.EG4.Detail = healthError, e.err
		h.EG4.Message = "Cannot communicate with the EG4 6000XP. Circuits are not being updated from live SOC or grid data."
	case e.lastOK.IsZero():
		h.EG4.State = healthWarning
		h.EG4.Message = "Waiting for the first reading from the EG4 6000XP."
	case now.Sub(e.lastOK) > stale:
		h.EG4.State = healthError
		h.EG4.Message = "EG4 6000XP data is stale. No successful reading recently."
	case !a.readEnabled:
		h.EG4.State = healthWarning
		h.EG4.Message = "Live EG4 read is off (test mode). SOC and grid values shown are simulated."
	default:
		h.EG4.State = healthOK
	}

	r := a.relayComm
	h.Relays.LastOK = fmtTime(r.lastOK)
	switch {
	case a.relayHWSimulated:
		h.Relays = CommStatus{State: healthWarning,
			Message: "Relay hardware is simulated. No relay module is being controlled; ON/OFF shows the software model."}
	case r.err != "":
		h.Relays.State, h.Relays.Detail = healthError, r.err
		h.Relays.Message = "Cannot communicate with the relay module. Circuits cannot be switched."
	default:
		// Not yet contacted counts as OK: the module is only polled when a
		// control pass runs, and a missing EG4 reading is reported above.
		h.Relays.State = healthOK
	}
	return h
}

func (a *App) Health() CommHealth {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthLocked(a.now())
}

// RunHealth re-evaluates health every second (staleness needs a clock, not
// just events) and broadcasts it to the dashboard whenever it changes.
func (a *App) RunHealth(ctx context.Context) {
	t := time.NewTicker(healthCheckEvery)
	defer t.Stop()
	var last CommHealth
	for {
		h := a.Health()
		if h != last {
			a.hub.Broadcast("health", h)
			last = h
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

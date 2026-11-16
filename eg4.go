package main

// EG4 6000XP telemetry over wired RS485 (Modbus RTU, FC4 input registers).
// Replaces the "EG4 6000XP Modbus" Node-RED tab.

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/goburrow/modbus"
)

// EG4Data mirrors global.eg4_data from the Node-RED flow (same JSON keys,
// so anything that consumed that object sees the same shape).
type EG4Data struct {
	PV1VoltageV          float64  `json:"pv1_voltage_v"`
	PV2VoltageV          float64  `json:"pv2_voltage_v"`
	BatteryVoltageV      float64  `json:"battery_voltage_v"`
	BatterySOCPct        float64  `json:"battery_soc_pct"`
	BatterySOHPct        float64  `json:"battery_soh_pct"`
	PV1PowerW            float64  `json:"pv1_power_w"`
	PV2PowerW            float64  `json:"pv2_power_w"`
	PVTotalPowerW        float64  `json:"pv_total_power_w"`
	EPSVoltageV          float64  `json:"eps_voltage_v"`
	EPSFrequencyHz       float64  `json:"eps_frequency_hz"`
	EPSTotalPowerW       float64  `json:"eps_total_power_w"`
	PV1EnergyTodayKWh    float64  `json:"pv1_energy_today_kwh"`
	PV2EnergyTodayKWh    float64  `json:"pv2_energy_today_kwh"`
	EnergyOutputTodayKWh float64  `json:"energy_output_today_kwh"`
	EPSL1PowerW          float64  `json:"eps_l1_power_w"`
	EPSL2PowerW          float64  `json:"eps_l2_power_w"`
	GridVoltageV         float64  `json:"grid_voltage_v"`
	GridFrequencyHz      float64  `json:"grid_frequency_hz"`
	GridImportW          float64  `json:"grid_import_w"`
	GridExportW          float64  `json:"grid_export_w"`
	BatteryChargeW       float64  `json:"battery_charge_w"`
	BatteryDischargeW    float64  `json:"battery_discharge_w"`
	BatteryPowerW        *float64 `json:"battery_power_w,omitempty"` // + charging, - discharging
	LastUpdated          string   `json:"last_updated,omitempty"`
}

// defaultEG4Data matches the "Get Current State" startup defaults.
func defaultEG4Data() EG4Data {
	return EG4Data{
		BatteryVoltageV: 52,
		BatterySOCPct:   100,
		BatterySOHPct:   100,
		EPSVoltageV:     240,
		EPSFrequencyHz:  60,
	}
}

// Grid is considered available when the inverter's AC input is inside these
// limits. They are deliberately wider than the inverter's own reconnect
// window: we only need to know that utility power is present.
const (
	gridMinVoltageV    = 200.0
	gridMaxVoltageV    = 270.0
	gridMinFrequencyHz = 57.0
	gridMaxFrequencyHz = 63.0
)

// BatteryCurrentA derives battery current from battery power and voltage:
// positive while charging, negative while discharging. ok is false when
// power or a usable voltage is missing.
func (d EG4Data) BatteryCurrentA() (amps float64, ok bool) {
	if d.BatteryPowerW == nil || d.BatteryVoltageV < 1 {
		return 0, false
	}
	return round(*d.BatteryPowerW/d.BatteryVoltageV, 1), true
}

// GridAvailable reports whether utility power is present on the inverter's
// grid input.
func (d EG4Data) GridAvailable() bool {
	return d.GridVoltageV >= gridMinVoltageV && d.GridVoltageV <= gridMaxVoltageV &&
		d.GridFrequencyHz >= gridMinFrequencyHz && d.GridFrequencyHz <= gridMaxFrequencyHz
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// bytesToRegs converts a Modbus register response to uint16 values.
func bytesToRegs(b []byte) []uint16 {
	regs := make([]uint16, len(b)/2)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(b[i*2:])
	}
	return regs
}

// applyBlock1 = "Parse Block 1 (PV / Battery / EPS / Energy)".
// regs[N] is input register N (read starts at address 0).
func applyBlock1(d *EG4Data, regs []uint16) error {
	if len(regs) < 36 {
		return fmt.Errorf("block 1: expected 40 registers, got %d", len(regs))
	}
	r := func(i int) float64 { return float64(regs[i]) }
	d.PV1VoltageV = round(r(1)*0.1, 1)
	d.PV2VoltageV = round(r(2)*0.1, 1)
	d.BatteryVoltageV = round(r(4)*0.1, 1)    // confirmed against the 6000XP display
	d.BatterySOCPct = float64(regs[5] & 0xFF) // low byte
	d.BatterySOHPct = float64(regs[5] >> 8)   // high byte
	d.PV1PowerW = r(7)
	d.PV2PowerW = r(8)
	d.PVTotalPowerW = r(7) + r(8)
	d.BatteryChargeW = r(10)    // Pcharge
	d.BatteryDischargeW = r(11) // Pdischarge
	p := r(10) - r(11)
	d.BatteryPowerW = &p
	d.GridVoltageV = round(r(12)*0.1, 1)     // VacR, L1-L2 on split phase
	d.GridFrequencyHz = round(r(15)*0.01, 2) // Fac
	d.EPSVoltageV = round(r(20)*0.1, 1)
	d.EPSFrequencyHz = round(r(23)*0.01, 2)
	d.EPSTotalPowerW = r(24)
	d.GridExportW = r(26) // Ptogrid
	d.GridImportW = r(27) // Ptouser
	d.PV1EnergyTodayKWh = round(r(28)*0.1, 1)
	d.PV2EnergyTodayKWh = round(r(29)*0.1, 1)
	d.EnergyOutputTodayKWh = round(r(35)*0.1, 1)
	return nil
}

// applyBlock2 = "Parse Block 2 (EPS L1/L2 Power)". Read starts at 125,
// so register 129 -> index 4, register 130 -> index 5.
func applyBlock2(d *EG4Data, regs []uint16) error {
	if len(regs) < 6 {
		return fmt.Errorf("block 2: expected 10 registers, got %d", len(regs))
	}
	d.EPSL1PowerW = float64(regs[4])
	d.EPSL2PowerW = float64(regs[5])
	return nil
}

type EG4Reader struct {
	handler *modbus.RTUClientHandler
	client  modbus.Client
}

func NewEG4Reader(port string, baud int, slaveID byte) *EG4Reader {
	h := modbus.NewRTUClientHandler(port)
	h.BaudRate = baud
	h.DataBits = 8
	h.Parity = "N"
	h.StopBits = 1
	h.SlaveId = slaveID
	h.Timeout = 1 * time.Second
	return &EG4Reader{handler: h, client: modbus.NewClient(h)}
}

func (r *EG4Reader) readRegs(addr, qty uint16) ([]uint16, error) {
	b, err := r.client.ReadInputRegisters(addr, qty)
	if err != nil {
		_ = r.handler.Close() // force a clean reopen on the next attempt; the read error is what matters
		return nil, fmt.Errorf("%s: %w", r.handler.Address, err)
	}
	return bytesToRegs(b), nil
}

// RunPoller replaces the modbus-read (every 5 s) -> parse -> getter -> parse
// -> "EG4 Enabled?" -> set global -> link out chain.
func (a *App) RunPoller(ctx context.Context, r *EG4Reader, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	defer func() {
		if err := r.handler.Close(); err != nil {
			log.Printf("eg4: closing port: %v", err)
		}
	}()
	failing := false

	for {
		a.pollOnce(r, &failing)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) pollOnce(r *EG4Reader, failing *bool) {
	regs1, err := r.readRegs(0, 40)
	a.recordEG4(err)
	if err != nil {
		if !*failing {
			log.Printf("eg4: read registers 0-39 failed: %v (will keep retrying quietly)", err)
			*failing = true
		}
		return
	}
	if *failing {
		log.Printf("eg4: communication restored")
		*failing = false
	}

	// Block 2 is chained after block 1 as in the flow. If it fails we still
	// publish block 1 (which carries SOC) rather than dropping the cycle.
	time.Sleep(5 * time.Millisecond)
	regs2, err2 := r.readRegs(125, 10)
	if err2 != nil {
		log.Printf("eg4: read registers 125-134 failed: %v", err2)
	}

	a.mu.Lock()
	if !a.readEnabled {
		// Test mode: live readings must not overwrite simulated data.
		a.mu.Unlock()
		return
	}
	d := a.data // copy, then merge (same merge semantics as the flow)
	if err := applyBlock1(&d, regs1); err != nil {
		a.mu.Unlock()
		log.Printf("eg4: %v", err)
		return
	}
	if err2 == nil {
		if err := applyBlock2(&d, regs2); err != nil {
			log.Printf("eg4: %v", err)
		}
	}
	d.LastUpdated = time.Now().UTC().Format(time.RFC3339Nano)
	a.data = d
	a.haveData = true
	a.noteGridLocked(d)
	a.mu.Unlock()

	a.hub.Broadcast("data", d)
	a.TriggerEvaluate()
}

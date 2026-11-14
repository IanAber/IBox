package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyBlock1(t *testing.T) {
	regs := make([]uint16, 40)
	regs[1] = 3505       // PV1 350.5 V
	regs[2] = 3410       // PV2 341.0 V
	regs[4] = 532        // 53.2 V
	regs[5] = 98<<8 | 76 // SOH 98, SOC 76
	regs[7], regs[8] = 2100, 1900
	regs[20] = 2401 // 240.1 V
	regs[23] = 6001 // 60.01 Hz
	regs[24] = 1500
	regs[28], regs[29], regs[35] = 123, 45, 67

	var d EG4Data
	if err := applyBlock1(&d, regs); err != nil {
		t.Fatal(err)
	}
	check := func(name string, got, want float64) {
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("pv1_voltage", d.PV1VoltageV, 350.5)
	check("pv2_voltage", d.PV2VoltageV, 341)
	check("battery_voltage", d.BatteryVoltageV, 53.2)
	check("soc", d.BatterySOCPct, 76)
	check("soh", d.BatterySOHPct, 98)
	check("pv_total", d.PVTotalPowerW, 4000)
	check("eps_voltage", d.EPSVoltageV, 240.1)
	check("eps_freq", d.EPSFrequencyHz, 60.01)
	check("eps_power", d.EPSTotalPowerW, 1500)
	check("pv1_kwh", d.PV1EnergyTodayKWh, 12.3)
	check("pv2_kwh", d.PV2EnergyTodayKWh, 4.5)
	check("out_kwh", d.EnergyOutputTodayKWh, 6.7)

	regs2 := make([]uint16, 10)
	regs2[4], regs2[5] = 800, 900
	applyBlock2(&d, regs2)
	check("l1", d.EPSL1PowerW, 800)
	check("l2", d.EPSL2PowerW, 900)
}

func TestHysteresis(t *testing.T) {
	cases := []struct {
		soc     float64
		current bool
		want    bool
	}{
		{85, false, true}, {80, false, true}, // at/above start -> on
		{70, false, false}, {70, true, true}, // between -> hold
		{60, true, false}, {40, true, false}, // at/below stop -> off
	}
	for _, c := range cases {
		if got := desiredState(c.soc, 80, 60, c.current); got != c.want {
			t.Errorf("soc %v current %v: got %v want %v", c.soc, c.current, got, c.want)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	good := `{"circuits":[` +
		`{"name":" Pool ","startSOC":"90","stopSOC":70},{"name":"","startSOC":80,"stopSOC":60},` +
		`{"startSOC":80,"stopSOC":60},{"startSOC":80,"stopSOC":60},{"startSOC":80,"stopSOC":60},` +
		`{"startSOC":80,"stopSOC":60},{"startSOC":80,"stopSOC":60},{"startSOC":80,"stopSOC":60}]}`
	cfg, err := validateConfig([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Circuits[0].Name != "Pool" || cfg.Circuits[0].StartSOC != 90 || cfg.Circuits[1].Name != "Circuit 2" {
		t.Errorf("unexpected: %+v", cfg.Circuits[:2])
	}
	if _, err := validateConfig([]byte(`{"circuits":[]}`)); err == nil {
		t.Error("expected error for wrong circuit count")
	}
	bad := `{"circuits":[{"startSOC":60,"stopSOC":60}` + `,{"startSOC":80,"stopSOC":60}`
	for i := 0; i < 6; i++ {
		bad += `,{"startSOC":80,"stopSOC":60}`
	}
	if _, err := validateConfig([]byte(bad + "]}")); err == nil {
		t.Error("expected error for start <= stop")
	}
}

func TestSimulatedControlLoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "circuit-config.json")
	app := NewApp(path, NewRelayModule("/dev/null-does-not-exist", 9600, 1), NewHub(), true)
	app.LoadConfigFromDisk() // missing file -> defaults written
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("defaults not written: %v", err)
	}

	// No data yet: evaluation must not act on the placeholder SOC of 100.
	if app.evaluateOnce() {
		t.Fatal("evaluated before any telemetry arrived")
	}

	app.SetReadEnabled(false) // test mode: also turns relay actions off
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 85})
	app.evaluateOnce()
	if app.relayStates[0] {
		t.Fatal("relay switched while relay actions were disabled")
	}

	app.SetRelayActionsEnabled(true)
	app.evaluateOnce()
	for i := 0; i < 8; i++ {
		if !app.relayStates[i] {
			t.Fatalf("circuit %d should be ON at 85%%", i+1)
		}
	}
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 70})
	app.evaluateOnce()
	if !app.relayStates[0] {
		t.Fatal("should hold ON between stop and start")
	}
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 55})
	app.evaluateOnce()
	if app.relayStates[0] {
		t.Fatal("should be OFF below stop")
	}
}

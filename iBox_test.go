package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyBlock1(t *testing.T) {
	regs := make([]uint16, 40)
	regs[1] = 3505       // PV1 350.5 V
	regs[2] = 3410       // PV2 341.0 V
	regs[4] = 532        // 53.2 V
	regs[5] = 98<<8 | 76 // SOH 98, SOC 76
	regs[7], regs[8] = 2100, 1900
	regs[10], regs[11] = 0, 1064 // discharging 1064 W
	regs[12] = 2425              // grid 242.5 V
	regs[15] = 5998              // grid 59.98 Hz
	regs[26], regs[27] = 300, 1200
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
	check("batt_discharge", d.BatteryDischargeW, 1064)
	if amps, ok := d.BatteryCurrentA(); !ok || amps != -20 { // -1064 W / 53.2 V
		t.Errorf("battery current = %v (ok %v), want -20", amps, ok)
	}
	check("grid_voltage", d.GridVoltageV, 242.5)
	check("grid_freq", d.GridFrequencyHz, 59.98)
	check("grid_export", d.GridExportW, 300)
	check("grid_import", d.GridImportW, 1200)
	if !d.GridAvailable() {
		t.Error("grid should be available at 242.5 V / 59.98 Hz")
	}

	regs2 := make([]uint16, block2Count)
	regs2[0], regs2[1], regs2[2] = 2398, 6002, 3100 // GEN 239.8 V, 60.02 Hz, 3100 W
	regs2[8], regs2[9] = 800, 900                   // registers 129, 130
	if err := applyBlock2(&d, regs2); err != nil {
		t.Fatal(err)
	}
	check("l1", d.EPSL1PowerW, 800)
	check("l2", d.EPSL2PowerW, 900)
	check("gen_voltage", d.GenVoltageV, 239.8)
	check("gen_freq", d.GenFrequencyHz, 60.02)
	check("gen_power", d.GenPowerW, 3100)

	regs2[0], regs2[2] = 0, 18500 // nothing on GEN: ignore register 123
	if err := applyBlock2(&d, regs2); err != nil {
		t.Fatal(err)
	}
	check("gen_power_no_voltage", d.GenPowerW, 0)
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

func TestGridAvailable(t *testing.T) {
	cases := []struct {
		v, hz float64
		want  bool
	}{
		{240, 60, true},
		{0, 0, false},    // grid down
		{12.3, 0, false}, // residual reading with grid down
		{240, 0, false},  // no frequency lock
		{180, 60, false}, // brownout
		{285, 60, false}, // overvoltage
		{240, 50, false}, // out-of-range frequency
	}
	for _, c := range cases {
		if got := (EG4Data{GridVoltageV: c.v, GridFrequencyHz: c.hz}).GridAvailable(); got != c.want {
			t.Errorf("%v V %v Hz: got %v want %v", c.v, c.hz, got, c.want)
		}
	}
}

func TestGridForcesAllRelaysOn(t *testing.T) {
	app := NewApp(filepath.Join(t.TempDir(), "c.json"), NewRelayModule("/dev/null-does-not-exist", 9600, 1), NewHub(), true)
	app.LoadConfigFromDisk()
	if app.cfg.GridStableSeconds != defaultGridStableSeconds {
		t.Fatalf("default grid stable time = %d", app.cfg.GridStableSeconds)
	}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return clock }
	app.cfg.GridStableSeconds = 60
	app.SetReadEnabled(false)
	app.SetRelayActionsEnabled(true)
	gridUp := EG4Data{BatterySOCPct: 20, GridVoltageV: 240, GridFrequencyHz: 60}

	// Low SOC, grid just appeared: still waiting.
	app.StoreSimulatedData(gridUp)
	app.evaluateOnce()
	if app.relayStates[0] {
		t.Fatal("circuit switched ON before grid was stable")
	}
	if st := app.lastStatus; !st.GridAvailable || st.GridOverride || st.GridStableRemaining != 60 {
		t.Fatalf("unexpected status while waiting: %+v", st)
	}

	// Grid blips off and back: the count restarts.
	clock = clock.Add(50 * time.Second)
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 20})
	clock = clock.Add(time.Second)
	app.StoreSimulatedData(gridUp)
	clock = clock.Add(30 * time.Second)
	app.evaluateOnce()
	if app.relayStates[0] || app.lastStatus.GridStableRemaining != 30 {
		t.Fatalf("blip should restart the count: on=%v status=%+v", app.relayStates[0], app.lastStatus)
	}

	// Stable for the full time: everything on.
	clock = clock.Add(30 * time.Second)
	app.evaluateOnce()
	for i := 0; i < 8; i++ {
		if !app.relayStates[i] {
			t.Fatalf("circuit %d should be ON once grid is stable", i+1)
		}
	}
	if !app.lastStatus.GridOverride {
		t.Error("status should report grid override")
	}

	// Grid drops with SOC between stop and start: hysteresis holds them on.
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 70})
	app.evaluateOnce()
	if !app.relayStates[0] {
		t.Fatal("should hold ON after grid loss while SOC is above stop")
	}

	// Grid still down and SOC below stop: off.
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 50})
	app.evaluateOnce()
	if app.relayStates[0] {
		t.Fatal("should be OFF without grid below stop SOC")
	}

	// Relay Actions disabled: grid must not switch anything.
	app.SetRelayActionsEnabled(false)
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 50, GridVoltageV: 240, GridFrequencyHz: 60})
	app.evaluateOnce()
	if app.relayStates[0] {
		t.Fatal("relay switched while relay actions were disabled")
	}
}

func TestValidateGridStableSeconds(t *testing.T) {
	circuits := strings.Repeat(`{"startSOC":80,"stopSOC":60},`, 7) + `{"startSOC":80,"stopSOC":60}`
	cases := []struct {
		field string
		want  int
		ok    bool
	}{
		{``, defaultGridStableSeconds, true}, // older file without the setting
		{`"gridStableSeconds":0,`, 0, true},
		{`"gridStableSeconds":"120",`, 120, true},
		{`"gridStableSeconds":-1,`, 0, false},
		{`"gridStableSeconds":1.5,`, 0, false},
		{`"gridStableSeconds":"",`, 0, false},
		{`"gridStableSeconds":86401,`, 0, false},
	}
	for _, c := range cases {
		cfg, err := validateConfig([]byte(`{` + c.field + `"circuits":[` + circuits + `]}`))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.field, err)
			continue
		}
		if c.ok && cfg.GridStableSeconds != c.want {
			t.Errorf("%s: got %d want %d", c.field, cfg.GridStableSeconds, c.want)
		}
	}
}

func TestGridTimerFiresEvaluation(t *testing.T) {
	app := NewApp(filepath.Join(t.TempDir(), "c.json"), NewRelayModule("/dev/null-does-not-exist", 9600, 1), NewHub(), true)
	app.LoadConfigFromDisk()
	app.cfg.GridStableSeconds = 1
	app.SetReadEnabled(false)
	app.SetRelayActionsEnabled(true)
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 20, GridVoltageV: 240, GridFrequencyHz: 60})
	for len(app.evalCh) > 0 {
		<-app.evalCh
	}
	app.evaluateOnce() // arms the timer
	select {
	case <-app.evalCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no re-evaluation after grid stable delay")
	}
	app.evaluateOnce()
	if !app.relayStates[0] {
		t.Fatal("circuit should be ON after the timer fired")
	}
}

func TestHealth(t *testing.T) {
	app := NewApp(filepath.Join(t.TempDir(), "c.json"), NewRelayModule("/dev/null-does-not-exist", 9600, 1), NewHub(), false)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return clock }
	app.eg4Polling = true
	app.pollEvery = 5 * time.Second

	state := func() (string, string) { h := app.Health(); return h.EG4.State, h.Relays.State }
	expect := func(what, wantEG4, wantRelay string) {
		t.Helper()
		if e, r := state(); e != wantEG4 || r != wantRelay {
			t.Errorf("%s: eg4=%s relays=%s, want %s/%s", what, e, r, wantEG4, wantRelay)
		}
	}

	expect("startup", healthWarning, healthOK)
	app.recordEG4(nil)
	expect("first reading", healthOK, healthOK)
	clock = clock.Add(16 * time.Second)
	expect("stale", healthError, healthOK)
	app.recordEG4(errors.New("serial: timeout"))
	if h := app.Health(); h.EG4.State != healthError || h.EG4.Detail != "serial: timeout" || h.EG4.LastOK == "" {
		t.Errorf("read error: %+v", h.EG4)
	}
	app.recordEG4(nil)
	expect("recovered", healthOK, healthOK)

	app.recordRelay(errors.New("no response"))
	expect("relay error", healthOK, healthError)
	app.recordRelay(nil)
	expect("relay recovered", healthOK, healthOK)

	app.SetRelayHWSimulated(true)
	app.SetReadEnabled(false)
	expect("test mode + simulated relays", healthWarning, healthWarning)

	app.eg4Polling = false
	expect("-no-eg4", healthWarning, healthWarning)
}

func TestRelayFailureRecordedByController(t *testing.T) {
	app := NewApp(filepath.Join(t.TempDir(), "c.json"), NewRelayModule("/dev/null-does-not-exist", 9600, 1), NewHub(), false)
	app.LoadConfigFromDisk()
	app.SetReadEnabled(false)
	app.StoreSimulatedData(EG4Data{BatterySOCPct: 50})
	if app.evaluateOnce() {
		t.Fatal("evaluation should fail without a relay module")
	}
	if h := app.Health(); h.Relays.State != healthError || h.Relays.Detail == "" {
		t.Errorf("relay failure not reported: %+v", h.Relays)
	}
}

func TestBatteryCurrentUnknown(t *testing.T) {
	if _, ok := (EG4Data{BatteryVoltageV: 52}).BatteryCurrentA(); ok {
		t.Error("no power reading should give no current")
	}
	p := 1000.0
	if _, ok := (EG4Data{BatteryPowerW: &p}).BatteryCurrentA(); ok {
		t.Error("zero voltage should give no current")
	}
}

func TestRawDumpText(t *testing.T) {
	var d rawDump
	d.values[12], d.ok[12] = 2405, true
	d.values[26], d.ok[26] = 0xFFCE, true // -50 as int16
	d.errs = []string{"registers 40-79: timeout"}
	out := d.text()
	for _, want := range []string{" 12      2405    2405  0965", " 26     65486     -50  FFCE", "ERROR registers 40-79: timeout"} {
		if !strings.Contains(out, want) {
			t.Errorf("dump text missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n  0 ") {
		t.Error("unread registers should be omitted")
	}

	app := NewApp(filepath.Join(t.TempDir(), "c.json"), NewRelayModule("/dev/null-does-not-exist", 9600, 1), NewHub(), true)
	if _, err := app.requestRawDump(context.Background()); err == nil {
		t.Error("dump should fail when EG4 polling is disabled")
	}
}

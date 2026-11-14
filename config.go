package main

// Persistent circuit configuration. Replaces the "Circuit SOC Configuration"
// tab: load at startup, validate, save, reload, fall back to defaults.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const numCircuits = 8

type Circuit struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	StartSOC float64 `json:"startSOC"`
	StopSOC  float64 `json:"stopSOC"`
}

type CircuitConfig struct {
	Version  int       `json:"version"`
	Circuits []Circuit `json:"circuits"`
}

func defaultConfig() CircuitConfig {
	c := CircuitConfig{Version: 1}
	for i := 0; i < numCircuits; i++ {
		c.Circuits = append(c.Circuits, Circuit{
			ID: i + 1, Name: fmt.Sprintf("Circuit %d", i+1), StartSOC: 80, StopSOC: 60,
		})
	}
	return c
}

// rawConfig accepts loosely typed input (numbers as strings, etc.) the same
// way the JS Number() calls did.
type rawConfig struct {
	Circuits []map[string]any `json:"circuits"`
}

func toNumber(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case bool:
		if x {
			return 1
		}
		return 0
	case nil:
		return 0 // Number(null) === 0 in JS
	}
	return math.NaN()
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// validateConfig = validate() in "Validate + store circuitConfig".
func validateConfig(raw []byte) (CircuitConfig, error) {
	var rc rawConfig
	if err := json.Unmarshal(raw, &rc); err != nil {
		return CircuitConfig{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(rc.Circuits) != numCircuits {
		return CircuitConfig{}, errors.New("Configuration must contain exactly 8 circuits.")
	}
	out := CircuitConfig{Version: 1}
	for i, src := range rc.Circuits {
		start := toNumber(src["startSOC"])
		stop := toNumber(src["stopSOC"])
		if !isFinite(start) || !isFinite(stop) || start < 0 || start > 100 || stop < 0 || stop > 100 {
			return CircuitConfig{}, fmt.Errorf("Circuit %d: SOC must be between 0 and 100.", i+1)
		}
		if start <= stop {
			return CircuitConfig{}, fmt.Errorf("Circuit %d: Start SOC must be greater than Stop SOC.", i+1)
		}
		name := ""
		if s, ok := src["name"]; ok && s != nil {
			name = strings.TrimSpace(fmt.Sprint(s))
		}
		if name == "" {
			name = fmt.Sprintf("Circuit %d", i+1)
		}
		out.Circuits = append(out.Circuits, Circuit{ID: i + 1, Name: name, StartSOC: start, StopSOC: stop})
	}
	return out, nil
}

// writeConfigFile writes atomically (temp file + rename) so a power cut
// mid-write can't leave a truncated config on the SD card.
func writeConfigFile(path string, cfg CircuitConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close() // already failing; report the write error
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // already failing; report the sync error
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadConfigFromDisk = startup inject / "Reload" button path.
//   - file missing or unparsable -> defaults, and defaults are written to disk
//   - file parses but fails validation -> defaults in memory, file left alone
func (a *App) LoadConfigFromDisk() {
	raw, err := os.ReadFile(a.configPath)
	if err == nil {
		var probe any
		err = json.Unmarshal(raw, &probe)
	}
	var cfg CircuitConfig
	writeDefaults := false
	if err != nil {
		log.Printf("config: cannot read %s (%v); using defaults", a.configPath, err)
		cfg = defaultConfig()
		writeDefaults = true
	} else if c, verr := validateConfig(raw); verr != nil {
		log.Printf("config: %v; using defaults", verr)
		cfg = defaultConfig()
	} else {
		cfg = c
	}

	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	a.hub.Broadcast("config", cfg)

	if writeDefaults {
		if err := writeConfigFile(a.configPath, cfg); err != nil {
			log.Printf("config: write defaults failed: %v", err)
		} else {
			log.Printf("config: wrote defaults to %s", a.configPath)
			a.TriggerEvaluate()
		}
	}
}

// SaveConfig = dashboard "Save Configuration" path.
func (a *App) SaveConfig(raw []byte) (CircuitConfig, error) {
	cfg, err := validateConfig(raw)
	if err != nil {
		log.Printf("config: rejected save: %v", err)
		return cfg, err
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	a.hub.Broadcast("config", cfg)

	if err := writeConfigFile(a.configPath, cfg); err != nil {
		log.Printf("config: write failed: %v", err)
		return cfg, fmt.Errorf("saved in memory but could not write file: %w", err)
	}
	log.Printf("config: saved to %s", a.configPath)
	a.TriggerEvaluate() // the flow re-ran load control after the file write
	return cfg, nil
}

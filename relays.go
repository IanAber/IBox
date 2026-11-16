package main

// Eight-channel Modbus RTU relay module (coils 0-7, FC1 read / FC5 write).

import (
	"fmt"
	"log"
	"time"

	"github.com/goburrow/modbus"
)

type RelayModule struct {
	handler *modbus.RTUClientHandler
	client  modbus.Client
}

func NewRelayModule(port string, baud int, slaveID byte) *RelayModule {
	h := modbus.NewRTUClientHandler(port)
	h.BaudRate = baud
	h.DataBits = 8
	h.Parity = "N"
	h.StopBits = 1
	h.SlaveId = slaveID
	h.Timeout = 1 * time.Second
	return &RelayModule{handler: h, client: modbus.NewClient(h)}
}

// ReadStates = "Read 8 Relay States" + "Save Actual Relay States".
func (m *RelayModule) ReadStates() ([8]bool, error) {
	var states [8]bool
	b, err := m.client.ReadCoils(0, 8)
	if err != nil {
		_ = m.handler.Close() // force a clean reopen on the next attempt; the read error is what matters
		return states, fmt.Errorf("%s: read coils: %w", m.handler.Address, err)
	}
	if len(b) < 1 {
		return states, fmt.Errorf("unexpected relay read response (%d bytes)", len(b))
	}
	for i := 0; i < 8; i++ {
		states[i] = b[0]&(1<<uint(i)) != 0
	}
	return states, nil
}

// WriteCoil = "Write Relay" (modbus-flex-write, FC5).
func (m *RelayModule) WriteCoil(addr int, on bool) error {
	v := uint16(0x0000)
	if on {
		v = 0xFF00
	}
	_, err := m.client.WriteSingleCoil(uint16(addr), v)
	if err != nil {
		_ = m.handler.Close() // force a clean reopen on the next attempt; the write error is what matters
		return fmt.Errorf("%s: write coil %d: %w", m.handler.Address, addr, err)
	}
	return nil
}

func (m *RelayModule) Close() {
	if err := m.handler.Close(); err != nil {
		log.Printf("relays: closing port: %v", err)
	}
}

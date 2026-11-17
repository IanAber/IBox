package main

// Raw EG4 register dump for working out the 6000XP register map in the
// field: GET /api/eg4/raw lists input registers 0-199 with their unsigned,
// signed and hex values. The reads run on the poller goroutine so they never
// interleave with normal polling on the serial port.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	rawDumpRegs  = 200
	rawDumpChunk = 40
)

type rawDump struct {
	values [rawDumpRegs]uint16
	ok     [rawDumpRegs]bool
	errs   []string
}

func (r *EG4Reader) dump() rawDump {
	var d rawDump
	for start := 0; start < rawDumpRegs; start += rawDumpChunk {
		regs, err := r.readRegs(uint16(start), rawDumpChunk)
		if err != nil {
			d.errs = append(d.errs, fmt.Sprintf("registers %d-%d: %v", start, start+rawDumpChunk-1, err))
			continue
		}
		for i, v := range regs {
			d.values[start+i] = v
			d.ok[start+i] = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return d
}

func (d rawDump) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "EG4 input registers, read %s\n", time.Now().Format(time.RFC3339))
	for _, e := range d.errs {
		fmt.Fprintf(&b, "ERROR %s\n", e)
	}
	b.WriteString("reg  unsigned  signed   hex\n")
	for i := 0; i < rawDumpRegs; i++ {
		if !d.ok[i] {
			continue
		}
		v := d.values[i]
		fmt.Fprintf(&b, "%3d  %8d  %6d  %04X\n", i, v, int16(v), v)
	}
	return b.String()
}

// requestRawDump asks the poller for a dump and waits for it.
func (a *App) requestRawDump(ctx context.Context) (rawDump, error) {
	if a.rawReq == nil {
		return rawDump{}, fmt.Errorf("EG4 polling is disabled (-no-eg4)")
	}
	reply := make(chan rawDump, 1)
	select {
	case a.rawReq <- reply:
	case <-ctx.Done():
		return rawDump{}, fmt.Errorf("poller busy: %w", ctx.Err())
	}
	select {
	case d := <-reply:
		return d, nil
	case <-ctx.Done():
		return rawDump{}, fmt.Errorf("timed out waiting for register dump: %w", ctx.Err())
	}
}

func (a *App) handleRawDump(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	d, err := a.requestRawDump(ctx)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintln(w, err)
		return
	}
	_, _ = w.Write([]byte(d.text()))
}

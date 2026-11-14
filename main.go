// IBox - Go port of the "Intelligence Box" Node-RED flows.
//
//   - Polls an EG4 6000XP over RS485 Modbus RTU every 5 s
//   - Switches 8 Modbus relays on/off from battery SOC with per-circuit
//     start/stop hysteresis
//   - Stores circuit settings in a JSON file
//   - Serves a web dashboard (configuration + test simulator)
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		eg4Port     = flag.String("eg4-port", "/dev/ttySC0", "serial port for the EG4 inverter (INV485)")
		eg4Baud     = flag.Int("eg4-baud", 19200, "EG4 baud rate")
		eg4Unit     = flag.Int("eg4-unit", 1, "EG4 Modbus unit ID")
		relayPort   = flag.String("relay-port", "/dev/ttySC1", "serial port for the relay module")
		relayBaud   = flag.Int("relay-baud", 9600, "relay module baud rate")
		relayUnit   = flag.Int("relay-unit", 1, "relay module Modbus unit ID")
		configPath  = flag.String("config", "/home/egadmin/circuit-config.json", "circuit configuration file")
		listen      = flag.String("listen", ":80", "HTTP listen address for the dashboard")
		poll        = flag.Duration("poll", 5*time.Second, "EG4 poll interval")
		hwSimulated = flag.Bool("relay-hw-simulated", true, "start with relay hardware simulated (no relay module connected)")
		noEG4       = flag.Bool("no-eg4", false, "do not poll the inverter at all (bench testing with the simulator)")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := NewHub()
	relays := NewRelayModule(*relayPort, *relayBaud, byte(*relayUnit))
	defer relays.Close()

	app := NewApp(*configPath, relays, hub, *hwSimulated)
	app.LoadConfigFromDisk()

	go app.RunController(ctx)

	if *noEG4 {
		log.Printf("eg4: polling disabled (-no-eg4); use the test simulator page")
	} else {
		eg4 := NewEG4Reader(*eg4Port, *eg4Baud, byte(*eg4Unit))
		go app.RunPoller(ctx, eg4, *poll)
		log.Printf("eg4: polling %s @ %d baud every %s", *eg4Port, *eg4Baud, *poll)
	}
	log.Printf("relays: %s @ %d baud (hardware simulated: %v)", *relayPort, *relayBaud, *hwSimulated)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx }, // ends SSE streams on shutdown
	}
	go func() {
		log.Printf("web: dashboard on http://%s/dashboard", displayAddr(*listen))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("web: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Printf("web: shutdown: %v", err)
	}
}

func displayAddr(listen string) string {
	if listen == ":80" {
		return "<pi-address>"
	}
	if len(listen) > 0 && listen[0] == ':' {
		return "<pi-address>" + listen
	}
	return listen
}

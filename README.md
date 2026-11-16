# IBox — Go port of the "Intelligence Box" Node-RED flows

One static binary that replaces all four Node-RED tabs:

| Node-RED tab | Go file |
|---|---|
| EG4 6000XP Modbus (poll regs 0-39 + 125-134 every 5 s) | `eg4.go` |
| SOC Load Control (8 relays, start/stop hysteresis, FC5 writes, 5 s retry) | `controller.go`, `relays.go` |
| Circuit SOC Configuration (JSON file, validate, save/reload, defaults) | `config.go` |
| Dashboard pages + EG4 Test Simulator | `web.go`, `web/index.html` (embedded in the binary) |

The dashboard is now on the standard web port 80: `http://<pi>/dashboard/configuration` and
`http://<pi>/dashboard/eg4-test` (`http://<pi>/` redirects to the dashboard). Use `-listen :1880`
to go back to the old Node-RED port. The config file format and path are unchanged, so an existing
`/home/egadmin/circuit-config.json` is picked up as-is.

## Install on the Pi

Pick the binary for your OS (`uname -m`: `aarch64` → arm64, `armv7l` → armv7, `armv6l` → armv6):

```bash
sudo systemctl disable --now nodered      # frees the serial ports
sudo install -m 755 iBox-linux-arm64 /usr/local/bin/iBox
sudo cp iBox.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now iBox
journalctl -u iBox -f                   # watch the log
```

The service runs as root, so it can open `/dev/ttySC0` / `/dev/ttySC1` and use port 80 with no
extra setup. Nothing else on the Pi may be using port 80 (check with `sudo ss -ltnp 'sport = :80'`;
stop Apache/nginx/lighttpd if one is installed). To run it by hand outside systemd, use `sudo ibox`.

Because it runs as root, a config saved from the dashboard is written as a root-owned file. Use
`sudo` if you want to edit `/home/egadmin/circuit-config.json` by hand.

## Options

```
-eg4-port /dev/ttySC0        -eg4-baud 19200   -eg4-unit 1
-relay-port /dev/ttySC1      -relay-baud 9600  -relay-unit 1
-config /home/egadmin/circuit-config.json
-listen :80                  -poll 5s
-relay-hw-simulated=true     start with the software relay model (as the flow did)
-no-eg4                      don't poll the inverter (bench testing with the simulator)
```

When the real relay module is wired up, either flip **Simulate Relay Hardware** off on the test page
or start with `-relay-hw-simulated=false` in the service file (the toggle, like Node-RED's
in-memory globals, resets on restart).

## Build from source

```bash
make            # all three Pi targets into dist/
make test
```

## API (used by the dashboard; handy for scripting)

```
GET  /api/state
POST /api/config                    body: {"circuits":[...8...]}
POST /api/config/reload
POST /api/sim/data                  body: EG4 data JSON (ignored while live read is on)
POST /api/sim/read-enable           body: {"enabled": true|false}
POST /api/sim/relay-actions         body: {"enabled": true|false}
POST /api/sim/relay-hw-simulated    body: {"enabled": true|false}
GET  /dashboard/events              Server-Sent Events: config, status, simstate, data
```

## Deliberate differences from the flow

1. **Live reads no longer overwrite test data.** In the flow, the Parse Block 1/2 functions modified
   `global.eg4_data` in place (it's an object reference), so live readings leaked through even with
   "Enable live EG4 read" off. Here, nothing is stored while live read is off.
2. **No relay action on placeholder data.** The flow seeded `eg4_data` with SOC = 100 at startup, so
   saving the config before the first real reading could switch every circuit ON. Control now waits
   for real or simulated telemetry.
3. **Simulated relay mode doesn't poll the relay module.** The flow always read the coils first, so
   with no module attached the read failed and evaluation never ran (and on success it overwrote the
   software model).
4. **Block 2 failure doesn't drop block 1.** SOC still updates if the 125-134 read fails.
5. **Test page mirrors live data continuously** while live read is on (the flow only refreshed it on
   toggle/page load).
6. **Config file is written atomically** (temp file + rename) to survive power loss.
7. **Grid input is read and overrides SOC control.** Input registers 12 (grid voltage, 0.1 V),
   15 (grid frequency, 0.01 Hz), 26 (export W) and 27 (import W) are parsed from the existing 0-39
   read. When grid has been present (200-270 V and 57-63 Hz) continuously for `gridStableSeconds`
   (config file / Configuration page, default 300, 0 = immediate; any grid-absent reading restarts
   the count) every relay is turned ON regardless of SOC. When grid drops, the normal start/stop hysteresis resumes from the all-on state, so circuits stay
   on until SOC falls to their Stop value. Relay Actions off still blocks all writes.

Unchanged: register map and scaling, hysteresis rule, relay retry every 5 s until read-back confirms,
100 ms gap between relay writes, config validation rules and messages, defaults
(8 × "Circuit N", start 80 / stop 60), entering test mode turns relay actions off and leaving it
turns them back on.

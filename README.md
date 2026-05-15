# LP-700 Server

WebSocket bridge for the **Telepost LP-500 / LP-700 Digital Station Monitor**.
One server owns the meter's USB HID handle; many clients (browsers, phones,
logging PCs) get live telemetry and can issue control verbs, with every
client kept in sync.

Companion of [VU3ESV/LP-100A-Server](https://github.com/VU3ESV/LP-100A-Server).
Same fan-out semantics, same WebSocket frame shape, same Pi-first deployment
story — HID instead of serial.

See [ARCHITECTURE.md](ARCHITECTURE.md) for the design and [CLAUDE.md](CLAUDE.md)
for the LP-500/700 USB HID protocol reference.

## Quick start (development, on a Mac)

```sh
go mod tidy
go test ./...
go run . -backend simulator -config deploy/config.example.toml
# open http://localhost:8089/
```

The reference web client is embedded in the binary — no separate frontend
build step.

## Building for the Raspberry Pi

```sh
./deploy/build-pi.sh                  # arm64 (Pi 3/4/5 with 64-bit OS)
ARCH=arm ./deploy/build-pi.sh         # ARMv7 (Pi Zero/1, 32-bit)
```

Pure Go (`/dev/hidraw` + sysfs on Linux), so no CGO toolchain is needed —
`GOOS=linux GOARCH=arm64 go build` works with whatever Go you have.

## Deploying on the Raspberry Pi

```sh
./deploy/redeploy.sh pi@raspberrypi.local
```

Self-bootstrapping: on a fresh Pi `redeploy.sh` creates the `lp700` system
user, installs the systemd unit + udev rule + default config, enables and
starts the service, and runs a `/healthz` check. Subsequent runs only push
the binary unless `--service` or `--keep-config` are explicitly used. SSH
multiplexing means you authenticate once per redeploy and `ssh -t` lets
sudo prompt interactively (or, with passwordless sudo, the deploy is silent).

```sh
./deploy/redeploy.sh pi@host --service       # also reinstall systemd unit
./deploy/redeploy.sh pi@host --keep-config   # binary only (skip toml/udev)
./deploy/redeploy.sh pi@host --restart-only  # bounce service only
ARCH=arm ./deploy/redeploy.sh pi@host        # 32-bit ARMv7 target
```

After install, confirm:

```sh
ssh pi@raspberrypi.local
systemctl status lp700-server
journalctl -u lp700-server -f
curl http://raspberrypi.local:8089/healthz
```

The udev rule matches the meter's Microchip default `04D8:0001` USB IDs
(plus a fallback on the Product string `"LP-500"` / `"LP-700"`). If your
meter uses different IDs, edit `/etc/udev/rules.d/99-lp700.rules` and run
`sudo udevadm control --reload-rules && sudo udevadm trigger`.

## CI / releases

- [`.github/workflows/ci.yml`](.github/workflows/ci.yml) — `go vet`,
  `go test -race`, `go build` on every push and PR.
- [`.github/workflows/release.yml`](.github/workflows/release.yml) —
  triggered by pushing a `v*.*.*` tag. Cross-compiles
  `linux/{amd64,arm64,armv7}` + `darwin/{amd64,arm64}` + `windows/amd64`,
  attaches `SHA256SUMS`, auto-generates release notes from the previous
  tag's commits.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

## Configuration

`/etc/lp700-server/config.toml`:

| Field                     | Default          | Notes                                                              |
|---------------------------|------------------|--------------------------------------------------------------------|
| `meter.backend`           | `auto`           | `auto`, `hid`, `simulator`                                         |
| `meter.vendor_id`         | `0x04D8`         | Microchip default; 0 = match by Product string                     |
| `meter.product_id`        | `0x0001`         | PIC32 firmware default                                             |
| `meter.poll_ms`           | `40`             | 25 Hz; the meter pushes IN reports faster than we read them        |
| `server.listen`           | `0.0.0.0:8089`   | `:8089` to leave 8088 for `LP-100A-Server`                         |
| `server.allow_control`    | `true`           | Set `false` for a read-only port                                   |
| `server.max_clients`      | `32`             |                                                                    |
| `server.heartbeat_ms`     | `2000`           |                                                                    |

After editing: `sudo systemctl restart lp700-server`.

### Logging

Defaults to **error** so the journal stays quiet. Two ways to turn it up:

- **At start time:** pass `-v` (debug). Edit `/etc/systemd/system/lp700-server.service`'s `ExecStart`.
- **At runtime, no restart:** open the web UI's **Setup** overlay and pick
  a level. Or hit the API directly:
  ```sh
  curl -s http://host:8089/api/log-level
  curl -s -XPOST http://host:8089/api/log-level -d '{"level":"debug"}'
  ```

## Diagnostic subcommands

```sh
sudo /usr/local/bin/lp700-server probe -list                          # enumerate every HID
sudo /usr/local/bin/lp700-server probe -dump                          # live raw + decoded frames
sudo /usr/local/bin/lp700-server probe -capture out.bin -duration 5s  # capture for offline analysis
```

These never run the WS server — they're hardware-only.

## Security

**There is no authentication.** Anyone with network access to the listen
address can read telemetry and issue control commands. This is intentional
for LAN-only deployment. For remote access, front the server with Tailscale
or WireGuard and bind the listen address to the VPN interface (or
`127.0.0.1`).

## HTTP / WebSocket surface

| Path             | Purpose                                                                                  |
|------------------|------------------------------------------------------------------------------------------|
| `/`              | Embedded reference web client                                                            |
| `/ws`            | Telemetry stream + control verbs (see [ARCHITECTURE.md §4](ARCHITECTURE.md))             |
| `/healthz`       | `ok\n` — for systemd / monitoring probes                                                 |
| `/api/config`    | `{backend, allow_control, title}` — UI bootstrap                                         |
| `/api/log-level` | GET / POST the running slog level (`error`/`warn`/`info`/`debug`)                        |

Smoke test from the command line:

```sh
# requires websocat (brew install websocat)
websocat ws://raspberrypi.local:8089/ws
echo '{"type":"command","id":"1","action":"peak_toggle"}' | websocat ws://raspberrypi.local:8089/ws
```

## Web client

Mirrors the LP-500/700 **Power/SWR** screen — Avg power, Peak power, SWR,
channel pills (Auto / 1..4), range cycle, Peak/Avg/Tune buttons, alarm
enable/tripped indicator.

The Waveform/'Scope and Spectrum modes are *not* mirrored to the web UI
(the Mac client in [LP-700-App](https://github.com/VU3ESV/LP-700-App)
renders them); the `mode_step` verb (still accepted on `/ws`) cycles
the meter's on-LCD display so an operator can switch into them remotely
if needed. While the meter is on the waveform or spectrum LCD page, the
server emits `{"type":"scope"}` / `{"type":"spectrum"}` WebSocket
frames at ~4 Hz alongside the regular `telemetry` stream — each carries
a 320-element 8-bit array assembled from the firmware's cmd-`'1'`..`'5'`
segments (see [CLAUDE.md](CLAUDE.md) "Scope and spectrum sample
buffers" and [ARCHITECTURE.md](ARCHITECTURE.md) §4).

Numeric alarm thresholds, callsign, coupler model, and firmware
revision live in the meter's NVRAM and aren't transmitted via USB at
all (confirmed by USB pcap audit — see [CLAUDE.md](CLAUDE.md)) so
those rows are absent from the UI.

## Operations cheatsheet

```sh
journalctl -u lp700-server -f                                         # tail logs
sudo systemctl restart lp700-server                                   # after config change
sudo systemctl disable --now lp700-server                             # stop / disable
curl -sf http://localhost:8089/healthz && echo OK                     # health check
sudo -u lp700 /usr/local/bin/lp700-server -config /etc/lp700-server/config.toml -v
                                                                       # foreground debug run
curl -s -XPOST http://localhost:8089/api/log-level -d '{"level":"debug"}'
                                                                       # bump verbosity live
```

## Layout

```
.
├── main.go                       # config -> hid -> hub -> http
├── internal/
│   ├── config/                   # TOML loader + defaults
│   ├── lpmeter/                  # HID owner, decoder, snapshot, simulator, probe
│   ├── hub/                      # WebSocket fan-out / fan-in / heartbeats
│   └── web/static/index.html     # reference web client (go:embed)
├── deploy/
│   ├── config.example.toml
│   ├── lp700-server.service      # systemd unit
│   ├── 99-lp700.rules            # udev rule (matches VID 0x04D8 PID 0x0001 + product string)
│   ├── build-pi.sh               # cross-compile from any Go-equipped host
│   ├── install.sh                # first-time install on the Pi
│   └── redeploy.sh               # subsequent updates (build + scp + restart, self-bootstraps)
├── examples/
│   └── node-red/                 # importable Node-RED flow + migration guide
└── .support/links.txt            # URLs to the external sources we decoded against
```

For Node-RED users coming from KD4Z's *LP-500/700 HID DIRECT* flow, see
[examples/node-red/](examples/node-red/) for a drop-in replacement that
talks to `/ws` instead of `/dev/hidraw*` (output `msg.payload` shape is
unchanged, so downstream nodes keep working).

## Acknowledgements

Telepost Inc. designed and manufactures the LP-500 and LP-700. This project
is unaffiliated; product names and trademarks belong to TelePost. The
protocol decoding was grounded in the manufacturer's VB6 source archive
(`LP-500_DataLogger` / `LP-500_VM`, downloadable from telepostinc.com),
a USB pcap capture of `LP-500_VM.exe` driving a real meter, and KD4Z's
Node-RED `LP Dice and Slice` parser. URLs in `.support/links.txt`.

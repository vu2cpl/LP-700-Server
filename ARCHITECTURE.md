# Architecture: LP-700 / LP-500 WebSocket Server

**Status:** v1 functional · **Repo:** [VU3ESV/LP-700-Server](https://github.com/VU3ESV/LP-700-Server)

## 1. Problem

The Telepost LP-500 and LP-700 Digital Station Monitors are four-channel
RF power meters with USB HID. The vendor's `LP-500_VM.exe` mirrors a
single meter to a single Windows desktop — bidirectional but
single-tenant, Windows-only.

Operators want the meter readable from a phone, a logging PC, and a
station dashboard *simultaneously*, with control actions reflected on
every screen the moment any one of them changes. That's what this server
provides — same shape as VU3ESV/LP-100A-Server but for the HID-based
LP-500/700 instead of the serial LP-100A.

## 2. Goals & non-goals

**Goals**

1. One server process owns the LP-500/700 HID handle.
2. Many WS clients receive a live diff-suppressed telemetry stream
   (channel, range, top mode, peak-mode, avg/peak power, SWR, alarm
   state, status messages).
3. Any client may issue named control verbs; resulting state changes
   broadcast to all clients within one poll cycle.
4. Pi (64-bit Raspberry Pi OS) is the primary target. Pure-Go build —
   no CGO, no toolchain dance.
5. Survives meter unplugs, cable yanks, client disappearance — auto-
   reconnect on both edges with exponential backoff.

**Non-goals (v1)**

- Mirroring the meter's Waveform/'Scope and Spectrum displays. Both push
  multi-kilobyte sample buffers; bandwidth isn't justified when the LCD
  is right there. The `mode_step` verb cycles the on-LCD mode; the v1
  web client only renders the Power/SWR view.
- Authentication. LAN-only, network trust only.
- Numeric alarm thresholds, callsign, coupler, firmware-rev. Confirmed
  by pcap audit that the firmware does not transmit these — see
  CLAUDE.md.

## 3. Architecture

```
   ┌─────────────────────┐
   │    LP-500 / LP-700  │  USB HID, vendor-specific class, 64-byte reports
   └──────────┬──────────┘
              │ /dev/hidraw* (Linux)
   ┌──────────▼─────────────────────────────────────────────┐
   │  lp700-server  (Go, single static binary, no CGO)      │
   │                                                         │
   │   ┌──────────────┐    ┌──────────────┐    ┌──────────┐ │
   │   │  HID owner   │───►│  State hub   │───►│  WS hub  │ │
   │   │ (poll '0' /  │◄───│  (last-known │◄───│ (fan-out,│ │
   │   │  '6', decode)│    │   snapshot)  │    │  fan-in) │ │
   │   └──────────────┘    └──────────────┘    └────┬─────┘ │
   └────────────────────────────────────────────────┴───────┘
                                                    │ WS /ws
              ┌────────────────────────┬────────────┼──────────┐
              ▼                        ▼            ▼          ▼
        Web dashboard           Logging PC       Phone     Station PC
```

Three goroutines, one snapshot:

1. **HID owner** — opens `/dev/hidraw*`, reads IN reports, alternates
   OUT polls between cmd `'0'` (live telemetry) and cmd `'6'` (status
   message refresh, every 10th tick). Writes parsed Snapshots to the
   state hub. Single-writer command queue interleaves client control
   verbs between polls.
2. **State hub** — keeps the most recent Snapshot + a monotonic seq.
   Diffs incoming snapshots and emits a `telemetry` event when any
   field changes; sends a heartbeat every N seconds when nothing has
   changed.
3. **WS hub** — accepts connections at `/ws`, sends each new client the
   current snapshot immediately, then streams events. Inbound control
   messages are validated and forwarded to the HID owner's queue.

## 4. WebSocket wire protocol

JSON text frames; `type` discriminates message kinds. Server→client
frames carry a monotonic `seq` so clients can detect drops.

**Server → client**

```json
// On connect, then on every changed field.
{
  "type": "telemetry",
  "seq": 12345,
  "ts": "2026-05-08T18:22:01.103Z",
  "data": {
    "channel": 1,                  // 1..4 (the locked-to channel when AutoChannel)
    "auto_channel": false,
    "power_avg_w": 94.9,
    "power_peak_w": 131.0,
    "swr": 1.06,
    "range": "100W",               // 5W | 10W | 25W | 50W | 100W | 250W | 500W | 1K | 2.5K | 5K | 10K | auto
    "peak_mode": "peak_hold",      // peak_hold | average | tune
    "alarm_enabled": true,
    "alarm_tripped": false,
    "top_mode": "power_swr",       // power_swr | waveform | spectrum | setup
    "status_message": ""           // e.g. "Reduce power or lower range" when set
    // power_mode, peak_hold_w, alarm_power_w, alarm_swr, callsign, coupler,
    // firmware_rev — present in the JSON shape for forward-compat, but
    // empty when reading real frames (firmware doesn't transmit them).
  }
}

{ "type": "heartbeat", "seq": 12346, "ts": "..." }
{ "type": "status", "level": "warn", "msg": "hid reopened after 1.3s gap" }
{ "type": "ack", "ref": "client-supplied-id", "ok": true }

// Emitted ~4 Hz while top_mode == "waveform" — full envelope buffer
// assembled from 5 HID segments. Not subject to CloseEnough dedup.
{
  "type": "scope",
  "seq": 12347,
  "ts": "2026-05-15T17:14:25.103Z",
  "data": {
    "top_mode": "waveform",
    "channel": 1,
    "auto_channel": false,
    "samples": [151, 151, 151, /* ... 320 u8 entries ... */]
  }
}

// Emitted ~4 Hz while top_mode == "spectrum" — full FFT bin buffer.
{
  "type": "spectrum",
  "seq": 12348,
  "ts": "2026-05-15T17:14:25.347Z",
  "data": {
    "top_mode": "spectrum",
    "channel": 1,
    "auto_channel": false,
    "bins": [77, 227, 127, /* ... 320 u8 entries ... */]
  }
}
```

Sample frames (`scope` and `spectrum`) carry 320 8-bit unsigned
values each, normalized for display height (the firmware auto-scales
the trace; the values are NOT absolute watts). For power readings,
use the matching `telemetry` frame. See CLAUDE.md "Scope and
spectrum sample buffers" for the underlying HID protocol.

**Client → server**

```json
{ "type": "command", "id": "abc-1", "action": "mode_step" }
{ "type": "command", "id": "abc-2", "action": "channel_step" }
{ "type": "command", "id": "abc-3", "action": "range_step" }
{ "type": "command", "id": "abc-4", "action": "alarm_toggle" }
{ "type": "command", "id": "abc-5", "action": "peak_toggle" }
{ "type": "command", "id": "abc-6", "action": "setup" }      // toggles in/out
{ "type": "command", "id": "abc-7", "action": "freeze" }     // scope/spec only

{ "type": "resync" }                                          // re-emit current snapshot
```

The server speaks named verbs; clients never see raw HID bytes. The
verb→opcode table can change in `internal/lpmeter/decode.go` without
breaking client code.

## 5. Configuration

Single TOML file; `-config <path>` overrides the default
`/etc/lp700-server/config.toml`. CLI flags: `-backend hid|simulator|auto`
and `-v` (start at debug log level).

```toml
[meter]
backend     = "auto"          # "hid" if a meter is enumerable, else "simulator"
vendor_id   = 0x04D8          # Microchip; 0 = match by product string
product_id  = 0x0001          # PIC32 firmware default
poll_ms     = 40              # 25 Hz; matches the LCD update cadence

[server]
listen        = "0.0.0.0:8089" # 8089 to coexist with LP-100A-Server on 8088
heartbeat_ms  = 2000
max_clients   = 32
allow_control = true           # set false for a read-only port

[ui]
title = "LP-500 / LP-700"
```

Default log level is **error** so the systemd journal stays quiet on a
Pi. `-v` starts at debug; `POST /api/log-level {"level":"debug"}` flips
it at runtime without restart.

## 6. Reference web client

Served from the same binary at `/`, embedded via `go:embed`. Mirrors
the meter's **Power/SWR** screen:

- **Average power** + **Peak power** — both always live (mirror the
  LCD's `57 AV / 94 PK` indicators)
- **SWR**
- **Channel pills** — Auto / 1 / 2 / 3 / 4 (highlighted = active)
- **Range** — single button cycling through the 12 settings
- **Peak / Avg / Tune** — three buttons (highlighted = active mode)
- **Alarm** pill — Disabled / Armed / TRIPPED (numeric thresholds note
  points to the meter LCD; firmware doesn't expose them)
- **Connection state** pill driven by heartbeat + `status` frames
- **Backend** indicator (HID vs SIMULATOR) — obvious when looking at
  synthesised data
- **Setup overlay** — runtime log-level picker

## 7. Failure modes

| Failure                          | Behaviour                                                        |
|----------------------------------|------------------------------------------------------------------|
| Device disappears (USB pull)     | Log warn, reconnect with 1s→30s exponential backoff. Clients see only heartbeats. |
| Bad/short frame                  | Drop, increment a parse-error counter, do not disconnect.       |
| Command-echo "ack" frame         | Filtered by `Decode()` (byte[0] != 0 ⇒ skip).                   |
| Client sends bad JSON            | NACK with `ok:false` if `id` known; close 1003 otherwise.       |
| `>max_clients` connect           | New connection gets 503; existing clients unaffected.           |
| Two clients press *Mode* at once | Both verbs queued FIFO; both get `ack`s; one telemetry update reflects the final state. |

## 8. Build + release

Pure Go; no CGO. From any host with Go installed:

```sh
GOOS=linux GOARCH=arm64 go build .       # Pi 3/4/5 (recommended)
GOOS=linux GOARCH=arm   GOARM=7 go build .  # 32-bit ARMv7 (Pi Zero/1)
```

`./deploy/build-pi.sh` wraps both. CI (`.github/workflows/ci.yml`) runs
vet + race tests + build on every push. Tagged releases
(`v*.*.*`) trigger `release.yml`, which cross-compiles all six targets
(linux amd64/arm64/armv7, darwin amd64/arm64, windows amd64) on a
single `ubuntu-latest` runner and publishes a GitHub Release with
SHA256SUMS.

## 9. Pi deployment

`./deploy/redeploy.sh pi@host` is self-bootstrapping: on a fresh Pi it
creates the `lp700` system user, installs the systemd unit, writes
`/etc/lp700-server/config.toml`, installs the udev rule (which grants
`plugdev` r/w on `/dev/hidraw*` matching VID 0x04D8 PID 0x0001), and
starts the service. Subsequent runs only push the binary unless
`--service` or `--keep-config` flags are passed. SSH multiplexing means
auth happens once per redeploy; `ssh -t` ensures sudo can prompt
interactively.

## 10. Out of scope, explicitly

- Cloud relay / NAT traversal — front the server with Tailscale,
  WireGuard, or similar.
- Logging/charting — a separate consumer can subscribe to `/ws` and
  write to InfluxDB.
- Replacing the Telepost VM. The vendor app stays usable; this server
  just needs the HID handle when running.
- Mirroring the Waveform/'Scope and Spectrum displays (see §2).
- Numeric alarm thresholds, callsign, coupler, firmware revision (see
  CLAUDE.md — pcap-confirmed not transmitted).

## 11. Hardware bring-up

For a fresh meter or a new firmware version:

```sh
# 1. Verify enumeration
sudo /usr/local/bin/lp700-server probe -list

# 2. Live frame view (raw hex + best-effort decode)
sudo /usr/local/bin/lp700-server probe -dump

# 3. Capture a window for offline analysis
sudo /usr/local/bin/lp700-server probe -capture lp500.bin -duration 10s
xxd lp500.bin | head -40

# 4. Verify control verbs round-trip
#    With the server running and the web UI open, click each control button
#    and confirm the meter's LCD advances exactly one step:
#      mode_step, channel_step, range_step, alarm_toggle, peak_toggle,
#      setup, freeze
```

If the live LCD readings don't match what the web UI shows, the
decoder offsets are wrong — adjust `internal/lpmeter/decode.go` against
the captured frame and re-test. The decoder is one isolated file
behind a stable Snapshot interface; everything else (hub, web client,
WS protocol) stays unchanged.

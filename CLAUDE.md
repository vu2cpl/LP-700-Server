# LP-700 Server

WebSocket bridge for the **Telepost LP-500 / LP-700 Digital Station Monitor**.
One server process owns the meter's USB HID handle; many clients (browsers,
phones, logging PCs) get live telemetry over WebSocket and may issue control
verbs. State changes from any client are reflected on every other client
within one poll cycle.

Sibling of [VU3ESV/LP-100A-Server][lp100a]. Same fan-out shape, same
deployment story; the only material difference is the meter link layer
(HID instead of VCP serial).

[lp100a]: https://github.com/VU3ESV/LP-100A-Server

## Repository layout

```
LP-700-Server/
├── CLAUDE.md            # this file
├── README.md            # build / install / operate
├── ARCHITECTURE.md      # design rationale + WebSocket wire protocol
├── main.go              # config → hid → hub → http
├── go.mod / go.sum
├── internal/
│   ├── config/          # TOML loader + defaults
│   ├── lpmeter/         # HID owner, frame decoder, snapshot model,
│   │                    #   simulator backend, probe diagnostics
│   ├── hub/             # WebSocket fan-out / fan-in / heartbeats
│   └── web/static/      # reference web client (embedded via go:embed)
├── deploy/              # build-pi.sh, install.sh, redeploy.sh,
│                        #   systemd unit, udev rule, config.example.toml
└── .support/
    └── links.txt           # URLs to vendor archive + companion repos (originals not in-tree)
```

## Stack decisions (locked in)

- **Language:** Go. Single static binary; no runtime to install.
- **HID layer:** **pure Go** via `/dev/hidraw*` + sysfs enumeration on Linux.
  No CGO, no `hidapi`, no `libudev`/`libusb` build deps. Cross-compiles
  cleanly with just `GOOS=linux GOARCH=arm64 go build`. macOS and Windows
  builds emit a stub that errors on `openHID` — the simulator backend
  works on every platform; a real meter requires a Linux host.
- **WebSocket:** `gorilla/websocket`.
- **Config:** TOML via `BurntSushi/toml`.
- **Logging:** `log/slog`, runtime-flippable via `/api/log-level`.
- **Deployment target:** 64-bit Raspberry Pi OS. Default port `:8089` so it
  coexists with `LP-100A-Server` on `:8088`.
- **Auth:** none. LAN-only deployment, document loudly.

## Backends

Picked via `meter.backend` in config or `-backend` flag:

| `auto`      | `hid` if a matching device is enumerable, else `simulator` (default) |
| `hid`       | Real LP-500/700. **Linux only.**                                     |
| `simulator` | Synthesised telemetry. Useful for UI work and demos without HW.      |

## LP-500 / LP-700 USB HID protocol

Grounded in three external sources (URLs in `.support/links.txt` —
originals not committed to keep the repo light):

1. **Manufacturer VB6 archive (LP-500_DataLogger / LP-500_VM)** —
   downloadable from `telepostinc.com`. `USB_HID_Functions.bas` shows
   the Win32 ReadFile/WriteFile convention (Report ID byte at
   buffer[0], 64-byte payload follows). `FrmSetup.frm` labels every
   documented field of the IN report and maps each F-button to its
   OUT command byte.
2. **USBPcap capture of `LP-500_VM.exe` driving a real meter.**
   Confirmed the wire conventions and revealed the cmd-`'6'`
   status-message slot.
3. **KD4Z's Node-RED `LP Dice and Slice` parser** — used to
   corroborate power/SWR scaling.

### Connection

- **VID:PID:** `0x04D8:0x0001` (Microchip, product 1).
- **Class:** Vendor-specific HID. Linux binds it via `hid-generic` and
  exposes `/dev/hidraw*`.
- **Reports:** 64-byte fixed payload, no Report ID (the OS prepends a
  zero RID byte transparently). Report data byte 0 holds either the
  command character (OUT) or the frame-type discriminator (IN).
- **Polling cadence:** ~25 Hz (40 ms). Host actively polls.

### OUT-report wire format

```
buf[0] = 0x00          ← Report ID prefix (added by writeReport())
buf[1..64] = 64-byte payload, where:
  payload[0] = ASCII command character (table below)
  payload[1..63] = zero
```

| verb            | command byte | source                              |
|-----------------|-------------:|-------------------------------------|
| poll            | `'0'` 0x30   | DataLogger `ReadAndWriteToDevice`   |
| status_query    | `'6'` 0x36   | pcap: triggers ASCII status at [40] |
| `mode_step`     | `'7'` 0x37   | DataLogger btn1 ('Mode')            |
| `channel_step`  | `'8'` 0x38   | DataLogger btn2 ('Channel')         |
| `range_step`    | `'9'` 0x39   | DataLogger btn3 ('Range')           |
| `alarm_toggle`  | `':'` 0x3A   | DataLogger btn4 (= F4 AL)           |
| `peak_toggle`   | `';'` 0x3B   | DataLogger btn5 (= F5 Peak/Avg/Tune)|
| `setup`         | `'<'` 0x3C   | DataLogger btn6 (= F6 Setup, toggles)|
| `freeze`        | `'?'` 0x3F   | DataLogger cmdFreeze                |

Cmds `'1'`–`'5'` retrieve scope/spectrum sample buffers — but unlike
cmds `'0'` and `'6'` they repurpose the **whole 64-byte IN report** as
sample data (NOT just bytes 40..63 — that "secondary slot" model only
applies to telemetry frames). See "Scope and spectrum sample buffers"
below.

**Firmware quirk — per-channel verbs in auto-channel mode:** `range_step`
(F3) and `alarm_toggle` (F4) are per-channel settings. When the meter is
in auto-channel mode (IN byte 4 == 0), the firmware silently ignores
these presses — there is no single "current channel" to mutate. The hub
intercepts these in `lpmeter.VerbAvailableInState` and NACKs with a
reason instead of writing a guaranteed-no-op OUT report; clients should
prompt the user to `channel_step` to a manual channel (CH1–4) first.
Confirmed empirically on 2026-05-09 by sending each verb 5× in auto-ch
mode and observing no change to offsets 6 / 7. `mode_step`, `channel_step`,
`peak_toggle`, `setup`, `freeze` all work in auto-ch.

### IN-report decode

The decoder rejects "ack" frames (any frame whose byte[0] is non-zero
— those are command-echo acks the firmware emits after each OUT
write). Real telemetry has byte[0] == 0.

Offsets used by the decoder:

| offset | field             | encoding                                                          |
|-------:|-------------------|-------------------------------------------------------------------|
|     0  | Peak HOLD Hi      | BE u16; `watts = raw * 0.2`. Firmware-maintained max-hold (what the LCD shows in Peak Hold mode); the DataLogger source mislabelled this as "Pk Pwr" — disambiguated by LP-500_VM v1.080 which dumps the same position as "Pk HLD_Pwr Hi byte =". |
|     1  | Peak HOLD Lo      |                                                                   |
|     2  | SWR high byte     | 16-bit BE split with offset 37; `swr = ((hi<<8) \| lo) / 100`     |
|     3  | top mode          | 0=Power/SWR, 1=Waveform, 2=Spectrum, 3=Setup                      |
|     4  | active channel    | 0=Auto, 1..4=CH1..CH4                                             |
|     5  | channel-auto      | when ch=0, the physical channel auto-mode is locked to (1..4)     |
|     6  | range index       | 0..10 = 5W..10KW, 11 = Auto. Per-channel; F3 is a no-op when byte 4 == 0 (auto-channel) — see firmware-quirk note above. |
|     7  | alarm DISABLED    | Inverted polarity: 0x00 = alarm armed on the LCD, 0x01 = alarm off. Confirmed empirically on 2026-05-08 by toggling F4 with channel set to manual CH1 (F4 is a no-op on CH-Auto for this firmware — same gating as F3 / range). The decoder negates the byte before populating `alarm_enabled`. |
|     8  | peak/avg/tune     | 0=Peak Hold, 1=Average, 2=Tune (verified on bench)                |
|    23  | Peak power Hi     | BE u16; `watts = raw * 0.2`. *Live* envelope peak this poll cycle; decays the moment the rig is unkeyed. (Distinct from offset 0-1's firmware-maintained Peak HOLD.) |
|    24  | Peak power Lo     |                                                                   |
|    25  | Avg power Hi      | BE u16; `watts = raw * 0.2`                                       |
|    26  | Avg power Lo      |                                                                   |
|    37  | SWR Lo            | see offset 2                                                      |
|  40–63 | secondary slot    | ASCII status message (after cmd `'6'`); else scope/spec samples   |

The decoder reads bytes 40..63 every frame and returns the text only when
it looks like printable ASCII (filters out the binary scope/spec
samples). The HID owner alternates the OUT poll: every 10th tick sends
`'6'` instead of `'0'` so the status slot is refreshed; the last
non-empty status sticks across plain telemetry frames so the UI doesn't
flicker.

### Scope and spectrum sample buffers (cmds `'1'`..`'5'`)

The firmware splits the on-LCD display buffer across 5 segments,
each delivered as a 64-byte IN report in response to OUT cmds
`'1'`..`'5'`. Concatenated in cmd order they form a single
**320-byte buffer**, 8-bit unsigned samples (the per-cmd boundary is
just a wire-protocol detail; the buffer is logically contiguous —
confirmed by spectral peaks straddling cmd boundaries).

What you get depends on the meter's current LCD page (`top_mode`):

| `top_mode` | cmd `'1'`..`'5'` IN frame contents                          |
|------------|--------------------------------------------------------------|
| `power_swr` | echo only (byte[0] = cmd char, rest zero — firmware refuses) |
| `waveform`  | envelope samples, 0..255 normalized to LCD trace height      |
| `spectrum`  | FFT-bin magnitudes, 0..255 normalized to LCD bar height      |
| `setup`     | echo only                                                    |

**Samples are normalized for display, not absolute units.** The
firmware auto-scales each trace so its peak fits the LCD (mirroring
the Teensy reference `f_page1.ino` / `g_page2.ino` logic). A steady
carrier produces a uniform-value scope buffer (e.g. all bytes = 151
in our 2026-05-15 probe at ~466 W). For absolute power, use the
matching telemetry frame's `power_avg_w` / `power_peak_w`.

**Refresh model.** Each segment cmd returns a static snapshot; the
buffer doesn't auto-update inside a short window of repeated reads.
The hub's HID owner polls one full cycle (cmd `'0'` then cmds
`'1'`..`'5'`, 6 ticks total ≈ 240 ms at the default 40 ms poll
cadence) and emits one `scope` / `spectrum` WS frame per assembly,
giving ~4 Hz refresh while the meter is on the matching LCD page.
During power_swr / setup modes the HID owner reverts to the legacy
cycle (cmd `'0'` every tick, cmd `'6'` every 10th).

**Sample rate / time base not yet measured.** Phase 1 reverse-
engineering captured uniform buffers (steady carrier into dummy
load). Sample rate of the scope buffer and absolute frequency-bin
spacing of the spectrum buffer require a CW key-up edge for timing
correlation — deferred. The current decoder treats samples and
bins as opaque ordered arrays; the Mac client renders shape.

### What the firmware does NOT expose over USB (definitive)

Verified by exhaustive search of the 5500-frame `LP700.pcapng`:

- Numeric Pwr/SWR alarm setpoints
- AL/SWR activation threshold
- Callsign
- Coupler model (LPC501/502/503/504/505)
- Firmware revision
- Per-channel alarm/coupler/range settings (CH 2–4)

None of these values appear in any IN frame in any plausible encoding.
Endpoint 0x82 isn't used by the LP-700 (different device); no control
transfers go to/from the meter. Even the manufacturer's DataLogger
source labels every field at offsets 1..35 and lists none of these.

These values live in the meter's NVRAM and are visible only on the LCD.
The Snapshot fields (`AlarmPowerW`, `AlarmSWR`, `Callsign`, `Coupler`,
`FirmwareRev`) remain in the JSON wire shape for forward compatibility
and are populated by the simulator backend, but the real decoder leaves
them empty and the web UI hides the rows that would have shown them.

## HTTP surface

| Path             | Method     | Purpose                                                       |
|------------------|------------|---------------------------------------------------------------|
| `/`              | GET        | Embedded reference web client                                 |
| `/ws`            | GET (WS)   | Telemetry stream + control verbs (see ARCHITECTURE.md §4)     |
| `/healthz`       | GET        | `ok\n` for systemd / monitoring probes                        |
| `/api/config`    | GET        | `{backend, allow_control, title}` — UI bootstrap              |
| `/api/log-level` | GET / POST | Read or change the running slog level (in-memory, no persist) |

## Diagnostic subcommands

```sh
sudo lp700-server probe -list                                # enumerate every HID
sudo lp700-server probe -dump                                # live raw + decoded frames
sudo lp700-server probe -capture out.bin -duration 5s        # capture for analysis
sudo lp700-server probe -samples -cycle-modes \
     -channel 1 -range 1K -frames-per-cmd 30                 # reverse-engineer
                                                             #   sample buffers
```

`-samples` cycles OUT cmds `'1'`..`'5'` across all three top_modes,
optionally driving the meter to a known channel/range first. Used to
reverse-engineer the wire format (see "Scope and spectrum sample
buffers" above). Requires the service to be stopped first
(`/dev/hidraw*` exclusive open):

```sh
sudo systemctl stop lp700-server
sudo lp700-server probe -samples -cycle-modes > probe.txt
sudo systemctl start lp700-server
```

See ARCHITECTURE.md §11 for how to use these on a fresh meter.

# Handover: enable LP-700 Scope (Page 1) and Spectrum (Page 2) wire frames

> **Status:** queued. The Mac client side ([VU3ESV/LP-700-App](https://github.com/VU3ESV/LP-700-App)) will pick up the new frame types once the server publishes them. Bulk of the work is in this repo.

## Goal

Surface the LP-500 / LP-700's **scope (waveform)** and **spectrum (FFT)** sample buffers over the existing `/ws` WebSocket so the Mac client can draw them. Today the meter sends sample data on the wire and the server silently drops it. App-side work is small once the wire format exists; the bulk is server-side decode + buffer assembly.

## What's already in this repo (skip these — they're done)

The server already **knows** about the sample buffers; it just deliberately ignores them in v1:

- **[`internal/lpmeter/decode.go:104–106`](../internal/lpmeter/decode.go#L104)** (`StatusReport()` comment):
  > "the Telepost VM cycles through cmd '0' (live telemetry) and cmd '6' (status text) along with `1`..`5` (**scope/spec sample buffers that v1 ignores**)"

- **[`internal/lpmeter/decode.go:40`](../internal/lpmeter/decode.go#L40)**:
  ```go
  cmdFreeze byte = '?' // 63 — Freeze (waveform/spectrum only)
  ```
  Confirms there's a running scope/spec for the firmware to freeze.

- **[`internal/lpmeter/decode.go:213, 222–227`](../internal/lpmeter/decode.go#L213)** (`extractStatusMessage()` comment):
  > "the slot also carries scope/spec sample data after cmd `0`..`5`. Scope/spec samples typically carry tiny binary values (0..3) which fail this filter."

  Bytes 40..63 of every IN report carry sample-slot data; `extractStatusMessage` keeps it only when it looks like ASCII text (i.e. after cmd `'6'`). After cmds `'0'..'5'` those 24 bytes are sample data and are currently discarded.

- **[`internal/lpmeter/owner.go:159–227`](../internal/lpmeter/owner.go#L159)**: current poll cycle is `cmd '0'` every tick + `cmd '6'` every 10th. Adding more cmd slots is a small change in the same loop.

## What's not known (the work)

**Sample-buffer formats for cmds `'1'..'5'`.** No public doc. The five commands probably split between scope (waveform / envelope) and spectrum (FFT) buffers, possibly with separate channels or buffer halves. Has to be reverse-engineered on the bench.

## Suggested sequence

### Phase 1 — Probe (the gating question)

Write a one-shot Go program (or add a `-probe-samples` flag to `main.go`) that:

1. Opens the meter.
2. For each cmd in `{'1','2','3','4','5'}`:
   - Writes the cmd as an OUT report.
   - Reads the next several IN reports (the slot at `[40:63]` should now carry sample data, not status text).
   - Dumps the slot as hex + decimal.
3. Logs the live `power_avg_w` alongside so you can correlate sample magnitudes with actual TX level.

Run with a **stable known TX source** (a tone into a dummy load is ideal). Look for:

- Is the slot one homogeneous run-length buffer, or multiple parallel channels?
- Is each sample 8-bit, 16-bit BE/LE? (Teensy reference uses 8-bit `ADfwd` for envelope, full-resolution magnitudes for FFT — see the Teensy code reference below.)
- Sample rate per cmd? (How many distinct samples per cmd response? Multiple cmd responses to fill one trace?)
- Does the meter only emit samples when `top_mode` matches (waveform / spectrum)? If yes, this is a **UX problem** — see "gotcha" below.

**Bench reference:** the Teensy SWR firmware that became the LP-700 is at `/Users/manoj/Desktop/TEENSY_SWR12082022_night/` on the maintainer's Mac. Pages 1 (`f_page1.ino`, 85 lines) and 2 (`g_page2.ino`, 84 lines + `fft.ino`) document what the decoded output should look like — 256-point envelope plot at ~3 kHz effective sample rate for scope, 12 kHz ADC → decimate to 3 kHz → 512-point FFT for spectrum.

### Phase 2 — Server decode + buffer assembly

Once the buffer layout is known:

1. **Add cmd slots in [`owner.go`](../internal/lpmeter/owner.go) poll cycle.** Today it's `cmd '0'` + `cmd '6'` every 10th. Insert e.g. `cmd '1'`/`'2'` at a fixed cadence — but be careful not to starve the telemetry rate (already ~25 Hz at the meter, gets dedupped by hub to ~5 Hz effective). One sample-cmd per ~5 telemetry polls = 5 Hz scope/spec refresh, probably enough for a UI.

2. **Add a decoder** (e.g. `decode_samples.go`) that reads `report[40:63]` after cmds `'1'..'5'` and assembles buffers across multiple reports. Output a complete scope frame or spectrum frame once enough partial buffers have arrived.

3. **Extend the hub broadcast** with new frame types:

   ```json
   {"type":"scope",   "seq":1234,"ts":"2026-05-13T...","data":{"samples":[...],"channel":1,"sample_rate":3000}}
   {"type":"spectrum","seq":1234,"ts":"2026-05-13T...","data":{"bins":[...],   "sample_rate":3000,"size":256}}
   ```

   Magnitudes should be normalised (0..1 floats) so the Mac client can scale freely. Sample rate + buffer size in the frame avoids any handshaking. Server-side hub dedup (`CloseEnough` in [`snapshot.go`](../internal/lpmeter/snapshot.go)) should **not** be applied to these — they change every frame.

4. **Add `KnownVerbs` entries / `EncodeCommand` cases** if any client-side commands need to flow back (e.g. `freeze`/`unfreeze` to map to the cmdFreeze byte). The verb name `freeze` is already in [`decode.go`](../internal/lpmeter/decode.go).

### Phase 3 — Mac client (small; handled in [LP-700-App](https://github.com/VU3ESV/LP-700-App))

Already scoped on the app side. Once the wire format exists:

- Add `.scope([Double])` and `.spectrum([Double])` to `ServerFrame` in `Sources/LP700App/Net/WireProtocol.swift`.
- New `WaveformView` + `SpectrumView` in `Sources/LP700App/Views/` modelled on the Teensy `f_page1.ino` / `g_page2.ino` rendering logic.
- Wire `top_mode` (already decoded in `Snapshot`) to switch the active view: `power_swr` → existing `PowerSWRView`, `waveform` → `WaveformView`, `spectrum` → `SpectrumView`, `setup` → `SetupOverlay`. The `mode_step` button already cycles `top_mode` server-side; the app just doesn't act on it today.

## Gotcha: `top_mode` dependency

Likely the meter only fills the scope buffer when its physical LCD is on the waveform page, and the spectrum buffer when it's on the spectrum page. The HID protocol is tied to the front panel.

If confirmed in the probe, two options:

1. **Server-mediated mode switch.** When a client requests scope frames, server sends `cmdMode` until `top_mode == "waveform"`. Disrupts the operator's local LCD view — bad UX.

2. **Follow the operator.** Server emits scope/spec frames only when the meter's `top_mode` matches naturally. Client UI reflects whatever the meter is displaying. Less flexible but doesn't fight the hardware.

Pick #2 unless there's a strong reason otherwise. The Mac app's view follows the meter, not the other way around.

## What the next session should NOT do

- **Don't add the wire frames before the probe completes.** The buffer formats are unknown; speculating will produce a schema that has to change.
- **Don't reuse the `telemetry` frame envelope.** Sample frames have very different cadence (5 Hz, full buffer each frame, not dedup-eligible) and a much larger payload — they'd evict legitimate telemetry from the same code path. New frame types, separate from the snapshot rate-limit.
- **Don't reintroduce WS-side leading-edge throttling on the client.** A v0.1.4 throttle there had to be removed in [LP-700-App PR #3](https://github.com/VU3ESV/LP-700-App/pull/3) because it stalled the UI on idle. If decode CPU ever becomes a concern for the new sample frames, throttle at the **emit** side with a trailing flush, not leading-edge filtering.

## Repro / testing

- **Server**: this repo. Build with `go build`; run against the real meter (HID backend) or `-backend simulator` (won't have real samples but proves the wire envelope).
- **Real meter**: deployed on `noderedpi4` at `192.168.1.169:8089`. Passwordless SSH+sudo set up for user `vu2cpl`. Healthcheck `GET /healthz` returns `ok`.
- **Mac client**: `/Users/manoj/projects/LP-700-App` (`main` tracks upstream `v0.1.5` plus open PR [#4](https://github.com/VU3ESV/LP-700-App/pull/4) — the hardware-style combined Power & SWR card).
- **Teensy reference firmware** showing what decoded samples should yield visually: `/Users/manoj/Desktop/TEENSY_SWR12082022_night/` on the maintainer's Mac. Pages 1 + 2 + `fft.ino` are the model for both the buffer math and the eventual Mac client renderer.

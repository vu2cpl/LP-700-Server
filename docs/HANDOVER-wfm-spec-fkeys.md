# Handover: expose the Waveform / Spectrum LCD-page function-key controls

> **Status:** queued. The Mac client ([VU3ESV/LP-700-App](https://github.com/VU3ESV/LP-700-App)) is shipping with a stub footnote in its WaveformView / SpectrumView pointing at the meter's front panel for the controls listed here. Once the server publishes verbs + snapshot fields for them, the client will grow real cycle-buttons in its ControlsCard.

## Background

The LP-500 / LP-700 has **context-sensitive F1–F6 function keys**: the labels and HID command bytes change depending on which LCD page is active. The server currently exposes only the Power/SWR-page set:

- `peak_toggle` (F1 in Power/SWR — peak hold / average / tune)
- `range_step` (F3 — range cycle)
- `alarm_toggle` (F4 — alarm enable / disable)
- `channel_step` (F2 — channel 0=auto, 1..4)
- `mode_step` (F5 — top-level LCD page cycle)
- `freeze` (F? — `cmdFreeze byte = '?'`, already in [`decode.go:40`](../internal/lpmeter/decode.go#L40) but no verb wired)

When the user hits F1 to switch the LCD to **Waveform** or **Spectrum**, the F1–F6 keys re-map on the meter's hardware to a different set. The server doesn't currently know about any of them.

## What the meter shows in Waveform mode

From the LP-700 hardware (operator confirmed; user is VU2CPL):

| F-key | Label | What it cycles |
|---|---|---|
| Top mode | LCD page (this is `mode_step`, already implemented) |
| **Ch** | Channel — same `channel_step` semantics as in Power/SWR |
| **Auto Rng / Rng** | Range — same `range_step` semantics |
| **Mode** | **Signal modulation type** — CW / SSB / PSK / etc (NOT the peak-mode peak_hold/average/tune from Power/SWR; that re-uses the F1 slot here) |
| **Wfm** | **Waveform subtype** — Wfm / Trap power scope / AM modulation / etc — selects what's being plotted into the scope buffer |
| **User 1 / User 2** | Operator presets / advanced settings |

## What the meter shows in Spectrum mode

Similar shape, different fifth-key:

| F-key | Label | What it cycles |
|---|---|---|
| Top mode | |
| **Ch** | Channel |
| **Auto Rng / Rng** | Range |
| **Mode** | Signal modulation type |
| **FFT** | **FFT window / span** — Hanning / Hamming / Blackman, span width, etc |
| **User 1 / User 2** | Operator presets |

## What the work looks like

### Phase 1 — Probe the HID command bytes (the gating question)

Same shape as the scope/spectrum sample-buffer probe ([HANDOVER-scope-spectrum.md](HANDOVER-scope-spectrum.md), which is done): get the HID command bytes the meter expects for each new F-key.

1. Switch the meter to Waveform LCD page (`mode_step` until `top_mode == "waveform"` per telemetry).
2. Press each F-key in turn on the **meter's front panel** while running a hex-dump capture against the meter's USB endpoint (`hidraw` on Linux, or `IOHIDDeviceRegisterInputReportCallback` on macOS). Each press should produce a paired OUT-then-IN report — the OUT byte at offset 0 is the F-key's command byte.
3. Repeat in Spectrum LCD page.

Likely candidates from the ASCII range that aren't already mapped:
- `cmdFreeze byte = '?' // 63` — already known but no verb
- `'1' ... '5'` — already used for sample-buffer reads (probably not these)
- `'8'` (56), `'@'` (64), `'A'` (65), `'B'`..., or higher

The meter's firmware likely overloads the same byte across pages (e.g. F4 = `cmdAlarm = ':'` in Power/SWR, but `:` in Waveform means "cycle Wfm subtype" — different action, same byte). If so, the server can re-use the existing bytes but with mode-aware semantics on the client.

### Phase 2 — Decode the current values into Snapshot

The Power/SWR snapshot already carries `peak_mode`, `power_mode`, `alarm_enabled`, etc. Extend it:

```go
// internal/lpmeter/snapshot.go
type Snapshot struct {
    // ... existing fields ...

    SignalMode    string `json:"signal_mode,omitempty"`    // cw | ssb | psk | am | fm | rtty | ...
    WaveformSub   string `json:"waveform_sub,omitempty"`   // wfm | trap | am_mod | ...
    SpectrumWindow string `json:"spectrum_window,omitempty"` // hanning | hamming | blackman | ...
    SpectrumSpan   int    `json:"spectrum_span_hz,omitempty"`
    UserPreset     int    `json:"user_preset,omitempty"`   // 0 = none, 1 or 2 active
}
```

Field-list comes from RE'd byte offsets in the IN reports. The IN-report layout in Waveform / Spectrum modes is currently unknown (only the sample-slot at `[40:63]` has been probed). Bytes 0..39 in those modes probably hold the values for the new F-keys.

### Phase 3 — New control verbs

```go
// internal/lpmeter/decode.go — KnownVerbs map
"signal_mode_step":  true,  // cycles CW / SSB / PSK / ...
"waveform_sub_step": true,  // cycles Wfm / Trap / AM mod / ... (only valid in waveform top_mode)
"spectrum_window_step": true,  // cycles FFT window (only valid in spectrum top_mode)
"user_preset":       true,  // arg 1..2 selects preset
"freeze":            true,  // already conceptually present via cmdFreeze
```

Encode each by mapping to the matching HID command byte discovered in Phase 1.

### Phase 4 — Gate verbs by `top_mode`

`signal_mode_step` and friends shouldn't work in Power/SWR mode — the meter would silently ignore them, but it's cleaner to NACK on the server. Pattern matches the existing [`VerbAvailableInState`](../internal/lpmeter/decode.go) function (extended in the auto-channel handover):

```go
func VerbAvailableInState(verb string, snap *Snapshot) (allowed bool, reason string) {
    // ... existing checks ...
    switch verb {
    case "waveform_sub_step":
        if snap.TopMode != "waveform" {
            return false, "waveform_sub_step is only valid on the Waveform LCD page"
        }
    case "spectrum_window_step":
        if snap.TopMode != "spectrum" {
            return false, "spectrum_window_step is only valid on the Spectrum LCD page"
        }
    }
    return true, ""
}
```

## Mac client work (handled in LP-700-App)

Once the server publishes the new verbs + snapshot fields:

1. Add the new fields to `Snapshot` in [`Sources/LP700App/Net/WireProtocol.swift`](LP-700-App/Sources/LP700App/Net/WireProtocol.swift).
2. Add the new `CommandAction` cases (`signalModeStep`, `waveformSubStep`, `spectrumWindowStep`, `userPreset`, `freeze`).
3. Extend `MeterViewModel` user-state with `userSignalMode`, `userWaveformSub`, `userSpectrumWindow`, `userPreset` (same sticky-user-state pattern as the existing controls).
4. Add corresponding `vm.sendSignalModeStep()` etc. methods.
5. In `ControlsCard.Style`:
   - `.sampleMode` becomes mode-aware (waveform vs spectrum)
   - Waveform: `CH / Rng / Mode (signal) / Wfm (subtype)` + maybe User 1/2 in a second row
   - Spectrum: `CH / Rng / Mode (signal) / FFT (window) / User 1/2`
6. Remove the placeholder footnote in WaveformView / SpectrumView.

## Repro / testing

- **Server**: this repo. The server-side scope/spectrum work (the prior handover) is the natural prerequisite; this builds on it.
- **Real meter**: `noderedpi4` at `192.168.1.169:8089`. Passwordless SSH+sudo for user `vu2cpl`.
- **Mac client**: `/Users/manoj/projects/LP-700-App` on branch `feat/scope-spectrum-views` (will be `main` after that branch merges upstream). The trace-area footnote currently reads e.g. _"Signal mode (CW/SSB/PSK), waveform subtype, and user presets are on the meter's front panel — server doesn't yet expose those commands."_ — that's the literal user-facing prompt this work will retire.

## What the next session should NOT do

- **Don't speculate on the command bytes.** Probe first; the F-key-to-byte mapping in Waveform / Spectrum modes is RE'd, not documented.
- **Don't reuse `peak_toggle` as the Waveform `Mode` (signal mode) verb.** Different actions, different bytes, different state. Stale aliasing will break the existing Power/SWR-mode `peak_toggle` semantics.
- **Don't gate sample-buffer emission on the new verb state.** Sample frames already gate on `top_mode == waveform/spectrum`; that's enough. Signal-mode / wfm-subtype are content selections within a page, not page-switches.

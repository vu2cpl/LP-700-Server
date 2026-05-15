package lpmeter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// ReportSize is the fixed length of every IN/OUT report on the LP-500/700.
// 64-byte vendor reports, no Report ID — the wire format is documented
// in the manufacturer's `LP-500_DataLogger` VB6 source (see CLAUDE.md
// for the source archive URL). Karalabe-style writes still need a
// Report ID byte prefixed (writeReport in owner.go does that), but the
// wire-level report itself is 64 bytes.
const ReportSize = 64

// LP-500/700 Microchip USB IDs. Confirmed in two independent sources:
//   - LP-500_DataLogger `USB_HID_Functions.bas:5-6`
//     (`MyVendorID = &H4D8`, `MyProductID = &H1`)
//   - KD4Z's Node-RED flow HIDConfig (`vid: "1240"`, `pid: "1"`;
//     decimal 1240 = 0x04D8).
const (
	DefaultVendorID  uint16 = 0x04D8
	DefaultProductID uint16 = 0x0001
)

// Wire-level command bytes the firmware accepts at byte 0 of an OUT
// report. Confirmed by the LP-500_DataLogger `FrmSetup.frm` button
// handlers (`OutputReportData(1) = N` after the RID prefix at
// `OutputReportData(0)`).
const (
	cmdPoll    byte = '0' // 48 — poll / get fresh telemetry
	cmdMode    byte = '7' // 55 — F1 Mode
	cmdChannel byte = '8' // 56 — F2 CH
	cmdRange   byte = '9' // 57 — F3 Rng
	cmdAlarm   byte = ':' // 58 — F4 AL
	cmdPeak    byte = ';' // 59 — F5 Peak / Avg / Tune
	cmdSetup   byte = '<' // 60 — F6 Setup (toggles enter/exit)
	cmdFreeze  byte = '?' // 63 — Freeze (waveform/spectrum only)
)

// IN-report byte offsets, 0-based against the 64-byte buffer that Linux
// hidraw delivers on Read (no RID prefix). Taken from the DataLogger
// source's InputReportData(N) indices minus 1.
//
// Source: LP-500_DataLogger `FrmSetup.frm:303-339` (vendor archive;
// see CLAUDE.md for URL).
const (
	// Bytes 0-1: Peak HOLD power (firmware-maintained max-hold), 16-bit
	// BE × 0.2 W. The DataLogger source labelled this "Pk Pwr Hi byte"
	// at InputReportData(1) — confirmed mislabelled by the LP-500_VM
	// v1.080 binary, which dumps the same position as "Pk HLD_Pwr Hi
	// byte =" right next to a separate "Pk Pwr Hi byte =" label for
	// the live-peak field at offset 23.
	OffsetPeakHoldHi  = 0
	OffsetPeakHoldLo  = 1
	OffsetSWRHi       = 2 // 16-bit BE, split with OffsetSWRLo (Node-RED-derived)
	OffsetTopMode     = 3 // 0=Power/SWR, 1=Waveform, 2=Spectrum, 3=Setup
	OffsetChannel     = 4 // 0=Auto, 1..4=CH1..CH4
	OffsetChannelAuto = 5 // when channel==0 (Auto), the physical channel auto is locked to (1..4)
	// OffsetRange: byte 6, 0..10 = 5W..10KW, 11 = Auto. Per-channel; the
	// F3 (Range) press is a no-op when byte 4 == 0 (auto-channel), same
	// gating as the alarm byte at offset 7 — see VerbAvailableInState.
	OffsetRange       = 6
	// OffsetAlarm: byte 7 is an alarm-DISABLED flag (inverted polarity).
	// 0x00 = alarm armed on the LCD, 0x01 = alarm off. The DataLogger
	// VB6 source labelled it "Alarm State" without specifying polarity;
	// confirmed inverted on 2026-05-08 by toggling F4 in CH1 manual mode
	// (F4 has no effect on Ch-Auto on this firmware) while logging
	// DEBUG. The decoder negates the byte before populating
	// AlarmEnabled, so the JSON/UI layer keeps a positive
	// "alarm_enabled" semantic.
	OffsetAlarm       = 7
	OffsetPeakAvg     = 8 // 0=peak_hold, 1=average, 2=tune
	OffsetAlarmSet    = 9 // alarm setpoint index (encoding TBD)
	OffsetPeakPwrHi   = 23 // 16-bit BE × 0.2 W — *live* envelope peak this poll cycle
	OffsetPeakPwrLo   = 24
	OffsetAvgPwrHi    = 25
	OffsetAvgPwrLo    = 26
	OffsetBGAvg       = 27 // bargraph display scaling — average
	OffsetBGPk        = 28 // bargraph display scaling — peak
	OffsetBGSWR       = 29 // bargraph display scaling — SWR
	OffsetPwrMultHi   = 30 // active full-scale watts; scale = (raw / 2)
	OffsetPwrMultLo   = 31
	OffsetPeakAvgMode = 32 // Peak/Avg display mode (distinct from peak hold cycle)
	OffsetFilter      = 33
	OffsetFreeze      = 34
	OffsetSWRLo       = 37 // hypothesis from the older Node-RED parser
)

// PollReport returns the 64-byte OUT report payload that asks the meter
// for a fresh telemetry frame. Byte 0 = command character ('0'), rest is
// zero. The HID owner sends this every tick.
func PollReport() []byte {
	out := make([]byte, ReportSize)
	out[0] = cmdPoll
	return out
}

// StatusReport returns the OUT payload that asks the meter to populate
// bytes 40..63 of the next telemetry frame with its current ASCII alert
// message ("Reduce power or lower range" etc.). Per the LP700.pcapng
// analysis the Telepost VM cycles through cmd '0' (live telemetry) and
// cmd '6' (status text) along with `1`..`5` (scope/spec sample buffers).
func StatusReport() []byte {
	out := make([]byte, ReportSize)
	out[0] = '6'
	return out
}

// SampleReport returns the OUT payload that asks the meter to deliver
// scope/spectrum buffer segment `seg` (1..5). The next IN report has
// the firmware-populated 64-byte segment payload (the WHOLE 64 bytes,
// not just bytes 40..63 — confirmed empirically 2026-05-15). Segments
// 1..5 concatenate in order to produce a single SampleBufferSize-byte
// buffer.
//
// Firmware ignores these cmds and returns an echo frame when the meter
// is NOT on the corresponding LCD page (waveform for envelope samples,
// spectrum for FFT bins). Power_SWR and Setup pages → echo only.
func SampleReport(seg byte) []byte {
	if seg < 1 || seg > 5 {
		panic(fmt.Sprintf("sample segment %d out of range 1..5", seg))
	}
	out := make([]byte, ReportSize)
	out[0] = '0' + seg // '1' .. '5'
	return out
}

// Decode parses a 64-byte IN report from the LP-500/700 into a Snapshot.
// Layout grounded in the manufacturer's DataLogger source.
func Decode(report []byte) (Snapshot, error) {
	if len(report) != ReportSize {
		return Snapshot{}, fmt.Errorf("expected %d-byte report, got %d", ReportSize, len(report))
	}

	// The firmware emits an ack/echo frame after every OUT command:
	// byte[0] = the command character we wrote (e.g. 0x30 for poll),
	// bytes [1..63] = 0. We can't filter on `byte[0] != 0` alone — that
	// also drops legitimate telemetry whenever the Peak-HOLD high byte
	// at offset 0 is non-zero (i.e. any held peak above 51.2 W). Match
	// the exact echo pattern instead: byte[0] is in the cmd-char range
	// AND every byte after it is zero.
	if report[0] >= '0' && report[0] <= '?' {
		echo := true
		for i := 1; i < ReportSize; i++ {
			if report[i] != 0 {
				echo = false
				break
			}
		}
		if echo {
			return Snapshot{}, errSkipFrame
		}
	}

	s := Snapshot{Timestamp: time.Now().UTC()}

	// Power: big-endian unsigned 16-bit, scale = ×0.2 W (raw * 2 / 10
	// per the Node-RED `LP Dice and Slice` function).
	//
	// Two distinct fields:
	//   bytes 0-1   → PeakHoldW (firmware-maintained max-hold; what
	//                 the LCD shows in Peak Hold mode)
	//   bytes 23-24 → PowerPeakW (envelope peak for this poll cycle;
	//                 decays the moment the rig is unkeyed)
	rawPeakHold := binary.BigEndian.Uint16(report[OffsetPeakHoldHi : OffsetPeakHoldHi+2])
	rawPeak := binary.BigEndian.Uint16(report[OffsetPeakPwrHi : OffsetPeakPwrHi+2])
	rawAvg := binary.BigEndian.Uint16(report[OffsetAvgPwrHi : OffsetAvgPwrHi+2])
	s.PeakHoldW = float64(rawPeakHold) * 0.2
	s.PowerPeakW = float64(rawPeak) * 0.2
	s.PowerAvgW = float64(rawAvg) * 0.2

	// SWR: big-endian split bytes (hi at offset 2, lo at offset 37),
	// scaled /100. Floor at 1.0 — SWR < 1.00 is impossible.
	rawSWR := uint16(report[OffsetSWRHi])<<8 | uint16(report[OffsetSWRLo])
	s.SWR = float64(rawSWR) / 100.0
	if s.SWR < 1.0 {
		s.SWR = 1.0
	}

	// Channel byte: 0=Auto, 1..4=CH1..CH4. When in Auto, byte 5 carries
	// the physical channel auto-mode is currently locked to (1..4).
	chByte := report[OffsetChannel]
	chAutoByte := report[OffsetChannelAuto]
	switch {
	case chByte == 0:
		s.AutoChannel = true
		if chAutoByte >= 1 && chAutoByte <= 4 {
			s.Channel = int(chAutoByte)
		} else {
			s.Channel = 1
		}
	case chByte >= 1 && chByte <= 4:
		s.AutoChannel = false
		s.Channel = int(chByte)
	default:
		return s, fmt.Errorf("channel byte 0x%02x out of range", chByte)
	}

	// Numeric alarm thresholds (Pwr W, SWR) live somewhere we haven't
	// pinned yet — likely served by a separate "get setup" command.
	// Leave them at zero so the UI shows — until we find them.

	// Range byte: 0..10 = 5W..10KW, 11 = Auto.
	rng := report[OffsetRange]
	if int(rng) >= len(rangeNames) {
		return s, fmt.Errorf("range byte 0x%02x out of range", rng)
	}
	s.Range = rangeNames[rng]

	// Top-level mode (0..3).
	if int(report[OffsetTopMode]) < len(topModeNames) {
		s.TopMode = topModeNames[report[OffsetTopMode]]
	} else {
		s.TopMode = "power_swr"
	}

	// Alarm + peak-mode are single bytes per the DataLogger map. Note
	// the inverted polarity on byte 7 — see OffsetAlarm comment.
	s.AlarmEnabled = report[OffsetAlarm] == 0
	if int(report[OffsetPeakAvg]) < len(peakModeNames) {
		s.PeakMode = peakModeNames[report[OffsetPeakAvg]]
	}

	// Bytes 40..63 are the secondary data slot whose content depends on
	// which OUT command was last sent. After cmd '6' it carries an ASCII
	// status message (e.g. "Reduce power or lower range" when SWR is
	// high). Detect ASCII-text frames by checking that the slot contains
	// printable bytes; ignore otherwise (scope/spec samples).
	s.StatusMessage = extractStatusMessage(report[40:])

	s.Valid = true
	return s, nil
}

// extractStatusMessage returns the trimmed ASCII message embedded in the
// secondary slot of a telemetry frame, or "" if the slot doesn't look
// like text (which is the common case — the slot also carries scope/spec
// sample data after cmd '0'..'5').
func extractStatusMessage(slot []byte) string {
	// The slot is text-ish when most of its non-zero bytes are printable
	// ASCII. Scope/spec samples typically carry tiny binary values
	// (0..3) which fail this filter.
	printable, nonzero := 0, 0
	for _, b := range slot {
		if b == 0 {
			continue
		}
		nonzero++
		if b >= 0x20 && b < 0x7f {
			printable++
		}
	}
	if nonzero < 4 || printable*4 < nonzero*3 {
		// Less than ~75% printable, or very sparse — not a message.
		return ""
	}
	out := make([]byte, 0, len(slot))
	for _, b := range slot {
		if b == 0 {
			break
		}
		if b >= 0x20 && b < 0x7f {
			out = append(out, b)
		}
	}
	// Trim trailing whitespace.
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// errSkipFrame signals that a structurally valid but non-telemetry frame
// should be ignored. Reserved for future use; the current single-frame-
// type protocol doesn't emit it.
var errSkipFrame = errors.New("non-telemetry frame, skipping")

// IsSkippable returns true for the sentinel errSkipFrame.
func IsSkippable(err error) bool { return errors.Is(err, errSkipFrame) }

// KnownVerbs is the set of control-verb names the server accepts on the
// WebSocket. Used by the hub to NACK unknown verbs before they reach the
// HID write queue. Keep in sync with EncodeCommand.
var KnownVerbs = map[string]bool{
	"mode_step":    true,
	"channel_step": true,
	"range_step":   true,
	"alarm_toggle": true,
	"peak_toggle":  true,
	"setup":        true,
	"freeze":       true,
}

// VerbAvailableInState returns "" if the firmware will act on the verb
// in the given state, or a reason string explaining why it will be
// silently ignored.
//
// LP-500/700 firmware quirk: F3 (Range) and F4 (Alarm) are per-channel
// settings. When the meter is in auto-channel mode (IN byte 4 == 0,
// snap.AutoChannel == true), the firmware ignores both presses — there
// is no single "current channel" to mutate. Confirmed empirically on
// 2026-05-09 by sending range_step / alarm_toggle 5× each in auto-ch
// mode and observing no change in offsets 6 / 7. The F4 case was
// already noted at decode.go OffsetAlarm; F3 has the same gating.
//
// snap may be nil (no telemetry observed yet on this connection), in
// which case we let the verb through and let the firmware decide.
func VerbAvailableInState(verb string, snap *Snapshot) string {
	if snap == nil {
		return ""
	}
	switch verb {
	case "range_step", "alarm_toggle":
		if snap.AutoChannel {
			return verb + " is ignored by firmware while in auto-channel mode; channel_step to a manual channel (CH1–4) first"
		}
	}
	return ""
}

// EncodeCommand renders a 64-byte OUT report payload for a single named
// control verb. Byte 0 is the command character; rest is zero. The
// `value` parameter is currently unused on the wire (the firmware's
// commands all advance by one step) but is preserved so a future "set
// channel directly to N" command can land without a client API break.
func EncodeCommand(verb string, value int) ([]byte, error) {
	out := make([]byte, ReportSize)
	switch verb {
	case "mode_step":
		out[0] = cmdMode
	case "channel_step":
		out[0] = cmdChannel
	case "range_step":
		out[0] = cmdRange
	case "alarm_toggle":
		out[0] = cmdAlarm
	case "peak_toggle":
		out[0] = cmdPeak
	case "setup":
		out[0] = cmdSetup
	case "freeze":
		out[0] = cmdFreeze
	default:
		return nil, fmt.Errorf("unknown verb %q", verb)
	}
	_ = value
	return out, nil
}

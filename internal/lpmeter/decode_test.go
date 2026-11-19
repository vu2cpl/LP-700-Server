package lpmeter

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"testing"
)

// buildSyntheticFrame produces a 64-byte report that matches the layout
// confirmed by the LP-500_DataLogger `FrmSetup.frm` source (vendor
// archive; URL in .support/links.txt).
func buildSyntheticFrame(s Snapshot) []byte {
	r := make([]byte, ReportSize)

	// Power: scale=×0.2W → raw = watts * 5.
	binary.BigEndian.PutUint16(r[OffsetAvgPwrHi:], uint16(s.PowerAvgW/0.2))
	binary.BigEndian.PutUint16(r[OffsetPeakPwrHi:], uint16(s.PowerPeakW/0.2))
	binary.BigEndian.PutUint16(r[OffsetPeakHoldHi:], uint16(s.PeakHoldW/0.2))

	// SWR: split bytes, scale=/100.
	rawSWR := uint16(s.SWR * 100)
	r[OffsetSWRHi] = byte(rawSWR >> 8)
	r[OffsetSWRLo] = byte(rawSWR & 0xff)

	// Bytes 30-31: the active full scale in half-watts. The meter always
	// writes one of its 11 range steps here, and Auto range picks one
	// too (5 W when idle), so isLikelyTelemetry requires it.
	fullScale := fullScaleHalfWatts[0]
	if i := indexOf(rangeNames, s.Range); i >= 0 {
		r[OffsetRange] = byte(i)
		if i < len(fullScaleHalfWatts) {
			fullScale = fullScaleHalfWatts[i]
		}
	}
	binary.BigEndian.PutUint16(r[OffsetPwrMultHi:], fullScale)
	if s.AutoChannel {
		r[OffsetChannel] = 0
		if s.Channel >= 1 && s.Channel <= 4 {
			r[OffsetChannelAuto] = byte(s.Channel)
		} else {
			r[OffsetChannelAuto] = 1
		}
	} else {
		r[OffsetChannel] = byte(s.Channel)
	}
	if i := indexOf(topModeNames, s.TopMode); i >= 0 {
		r[OffsetTopMode] = byte(i)
	}
	// Wire polarity is inverted: byte 7 is an alarm-DISABLED flag, so
	// alarm-enabled frames carry 0x00 here and disabled frames carry
	// 0x01. See OffsetAlarm comment in decode.go.
	if !s.AlarmEnabled {
		r[OffsetAlarm] = 1
	}
	if i := indexOf(peakModeNames, s.PeakMode); i >= 0 {
		r[OffsetPeakAvg] = byte(i)
	}

	return r
}

func indexOf(table []string, v string) int {
	for i, s := range table {
		if s == v {
			return i
		}
	}
	return -1
}

func TestDecodeRoundTripWireFields(t *testing.T) {
	want := Snapshot{
		Channel:      2,
		AutoChannel:  false,
		PowerAvgW:    100,
		PowerPeakW:   140,
		SWR:          1.06,
		Range:        "100W",
		TopMode:      "power_swr",
		AlarmEnabled: true,
		PeakMode:     "peak_hold",
	}
	got, err := Decode(buildSyntheticFrame(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Valid {
		t.Fatal("decoded snapshot is marked invalid")
	}
	if got.Channel != want.Channel {
		t.Errorf("channel: got %d, want %d", got.Channel, want.Channel)
	}
	if got.AutoChannel != want.AutoChannel {
		t.Errorf("auto: got %v, want %v", got.AutoChannel, want.AutoChannel)
	}
	if !floatNear(got.PowerAvgW, want.PowerAvgW, 0.2) {
		t.Errorf("power avg: got %v, want %v", got.PowerAvgW, want.PowerAvgW)
	}
	if !floatNear(got.PowerPeakW, want.PowerPeakW, 0.2) {
		t.Errorf("power peak: got %v, want %v", got.PowerPeakW, want.PowerPeakW)
	}
	if !floatNear(got.SWR, want.SWR, 0.01) {
		t.Errorf("swr: got %v, want %v", got.SWR, want.SWR)
	}
	if got.Range != want.Range {
		t.Errorf("range: got %s, want %s", got.Range, want.Range)
	}
	if got.TopMode != want.TopMode {
		t.Errorf("top mode: got %s, want %s", got.TopMode, want.TopMode)
	}
	if got.AlarmEnabled != want.AlarmEnabled {
		t.Errorf("alarm: got %v, want %v", got.AlarmEnabled, want.AlarmEnabled)
	}
	if got.PeakMode != want.PeakMode {
		t.Errorf("peak_mode: got %s, want %s", got.PeakMode, want.PeakMode)
	}
}

func TestDecodeAutoChannel(t *testing.T) {
	got, err := Decode(buildSyntheticFrame(Snapshot{
		AutoChannel: true, Channel: 3,
		PowerAvgW: 50, PowerPeakW: 70, SWR: 1.10, Range: "100W",
	}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.AutoChannel || got.Channel != 3 {
		t.Errorf("auto/channel: got auto=%v ch=%d, want auto=true ch=3", got.AutoChannel, got.Channel)
	}
}

// The Waveform and Spectrum pages have their own CH 1..4 setting (no
// Auto); byte 5 carries it and byte 4 stays 0 while Power/SWR is in CH
// Auto. Frames captured on an LP-700 on 2026-11-19 after CH presses on
// those pages; the LCD showed CH 4, then CH 2.
func TestDecodeSamplePageChannelIsNeverAuto(t *testing.T) {
	tests := []struct {
		name, hex string
		top       string
		ch        int
	}{
		{"waveform page, LCD CH 4", "0000030100040b010000040a0000090000000102040000000000002e0130000a0000000000e70000e37b06b784", "waveform", 4},
		{"spectrum page, LCD CH 2", "0000000200020b010000010a0000090000000202040000000000002e0030000a0000000000640000e37b06b784", "spectrum", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame, err := hex.DecodeString(tt.hex + strings.Repeat("00", ReportSize-len(tt.hex)/2))
			if err != nil {
				t.Fatal(err)
			}
			if frame[OffsetChannel] != 0 {
				t.Fatalf("fixture: byte 4 = %d, want 0 (the case under test)", frame[OffsetChannel])
			}
			got, err := Decode(frame)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.TopMode != tt.top || got.AutoChannel || got.Channel != tt.ch {
				t.Errorf("got top=%s auto=%v ch=%d, want top=%s auto=false ch=%d",
					got.TopMode, got.AutoChannel, got.Channel, tt.top, tt.ch)
			}
		})
	}

	// Same bytes 4/5 on the Power/SWR page still mean CH Auto locked to N.
	powerAuto := buildSyntheticFrame(Snapshot{AutoChannel: true, Channel: 2, TopMode: "power_swr", Range: "auto"})
	got, err := Decode(powerAuto)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.AutoChannel || got.Channel != 2 {
		t.Errorf("power_swr page: got auto=%v ch=%d, want auto=true ch=2", got.AutoChannel, got.Channel)
	}
}

// The Spectrum page carries its "Pwr" readout in 0.018 W steps. Frame
// captured on the LP-700 (2026-11-19) while the LCD showed 5 W / +37 dBm;
// the second case sets the raw value seen while it showed 185 W / +52 dBm.
func TestDecodeSpectrumPowerScale(t *testing.T) {
	const captured = "011e000200010501020001000000090000000102050100011e000001002a01f40200000000740000e37b06b784"
	frame, err := hex.DecodeString(captured + strings.Repeat("00", ReportSize-len(captured)/2))
	if err != nil {
		t.Fatal(err)
	}
	near := func(got, want float64) bool { return math.Abs(got-want) < 0.01 }

	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TopMode != "spectrum" || !near(got.PowerPeakW, 5.148) || !near(got.PeakHoldW, 5.148) {
		t.Errorf("5 W case: got top=%s peak=%.3f hold=%.3f, want spectrum 5.148 5.148",
			got.TopMode, got.PowerPeakW, got.PeakHoldW)
	}

	binary.BigEndian.PutUint16(frame[OffsetPeakPwrHi:], 10303)
	got, err = Decode(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !near(got.PowerPeakW, 185.454) {
		t.Errorf("185 W case: got peak=%.3f, want 185.454", got.PowerPeakW)
	}

	// The same bytes on the Waveform page keep the 0.2 W scale.
	frame[OffsetTopMode] = 1
	got, err = Decode(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TopMode != "waveform" || !near(got.PowerPeakW, 2060.6) {
		t.Errorf("waveform page: got top=%s peak=%.1f, want waveform 2060.6", got.TopMode, got.PowerPeakW)
	}
}

func TestDecodeSWRFloorsAtOne(t *testing.T) {
	r := buildSyntheticFrame(Snapshot{Channel: 1, Range: "100W", SWR: 0.5})
	got, err := Decode(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SWR != 1.0 {
		t.Errorf("expected swr floor at 1.0 (raw < 100), got %v", got.SWR)
	}
}

func TestDecodeRejectsShortFrame(t *testing.T) {
	if _, err := Decode(make([]byte, ReportSize-1)); err == nil {
		t.Fatal("expected error for short frame")
	}
}

func TestDecodeRejectsBadChannel(t *testing.T) {
	r := buildSyntheticFrame(Snapshot{Channel: 1, Range: "100W"})
	r[OffsetChannel] = 7
	if _, err := Decode(r); err == nil {
		t.Fatal("expected error for out-of-range channel byte")
	}
}

func TestDecodeRejectsBadRange(t *testing.T) {
	r := buildSyntheticFrame(Snapshot{Channel: 1, Range: "100W"})
	r[OffsetRange] = 99
	if _, err := Decode(r); err == nil {
		t.Fatal("expected error for out-of-range range byte")
	}
}

func TestEncodeCommandUnknownVerb(t *testing.T) {
	if _, err := EncodeCommand("nope", 0); err == nil {
		t.Fatal("expected error for unknown verb")
	}
}

func TestEncodeCommandLayout(t *testing.T) {
	// Per the DataLogger source: command character at byte[0] of the
	// 64-byte payload (the RID prefix is added by writeReport, not here).
	cases := map[string]byte{
		"mode_step":    '7',
		"channel_step": '8',
		"range_step":   '9',
		"alarm_toggle": ':',
		"peak_toggle":  ';',
		"setup":        '<',
		"freeze":       '?',
	}
	for verb, want := range cases {
		out, err := EncodeCommand(verb, 0)
		if err != nil {
			t.Errorf("%s: %v", verb, err)
			continue
		}
		if len(out) != ReportSize {
			t.Errorf("%s: got %d-byte report, want %d", verb, len(out), ReportSize)
		}
		if out[0] != want {
			t.Errorf("%s: byte[0]=0x%02x, want 0x%02x", verb, out[0], want)
		}
	}
}

// TestDecodePeakHold is the regression for "Peak Hold not working in
// the web UI" — bytes 0-1 carry the firmware-maintained held peak.
// The old decoder both ignored those bytes (always read live peak from
// 23-24) and dropped the entire frame whenever byte[0] != 0, which
// silently lost telemetry above 51.2 W of held peak.
func TestDecodePeakHold(t *testing.T) {
	// 250 W held peak: raw 1250 = 0x04E2 → bytes 0-1 = 0x04 0xE2.
	r := buildSyntheticFrame(Snapshot{
		Channel: 1, Range: "1K", SWR: 1.10,
		PowerAvgW: 100, PowerPeakW: 140, PeakHoldW: 250,
	})
	got, err := Decode(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !floatNear(got.PeakHoldW, 250, 0.2) {
		t.Errorf("peak hold: got %v, want 250 (bytes 0-1 must decode independently of bytes 23-24)", got.PeakHoldW)
	}
	if !floatNear(got.PowerPeakW, 140, 0.2) {
		t.Errorf("live peak: got %v, want 140", got.PowerPeakW)
	}
	// Sanity: byte[0] is 0x04 here. The old filter would have dropped
	// this frame; the new filter must let it through.
	if r[0] == 0 {
		t.Fatalf("test frame setup wrong: byte[0]=0, expected non-zero peak-hold high byte")
	}
}

func TestDecodeRejectsCommandEcho(t *testing.T) {
	// Real command echo: cmd char at byte 0, every other byte 0.
	for _, cmd := range []byte{'0', '6', '7', '8', '9', ':', ';', '<', '?'} {
		r := make([]byte, ReportSize)
		r[0] = cmd
		_, err := Decode(r)
		if !IsSkippable(err) {
			t.Errorf("cmd echo 0x%02x: expected skippable, got err=%v", cmd, err)
		}
	}
	// Real telemetry frame whose byte[0] happens to be in the cmd-char
	// range (high byte of a large peak-hold value) must NOT be skipped.
	r := buildSyntheticFrame(Snapshot{
		Channel: 1, Range: "1K", SWR: 1.10, PeakHoldW: 2500, // raw 12500 = 0x30D4
	})
	if r[0] < '0' || r[0] > '?' {
		t.Skipf("test setup didn't put a cmd-char in byte[0] (got 0x%02x); reconsider raw value", r[0])
	}
	if _, err := Decode(r); err != nil {
		t.Errorf("real telemetry with cmd-char-shaped peak-hold high byte must decode, got err=%v", err)
	}
}

func TestExtractStatusMessage(t *testing.T) {
	mk := func(s string) []byte {
		slot := make([]byte, 24) // bytes 40..63 are 24 bytes
		copy(slot, []byte(s))
		return slot
	}
	tests := []struct {
		name string
		slot []byte
		want string
	}{
		{"empty slot", mk(""), ""},
		{"single char", mk("X"), ""},
		{"too short", mk("hi"), ""},
		{"real status — multi-word", mk("Reduce power "), "Reduce power"},
		{"real status — TX Match req'd", mk("TX Match req'd"), "TX Match req'd"},
		{"no space → reject (single token)", mk("ReducePower"), ""},
		{"has space but no 3-letter run", mk("a b c d"), ""},
		{"sample-leak garbage (no space, no letter run)",
			mk("?BFILORUY|_bfilorvy|"), ""},
		{"alphabetised punctuation garbage (has letters but no space, no run)",
			mk("@AaBb)CcDdEe"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractStatusMessage(tt.slot)
			if got != tt.want {
				t.Errorf("extractStatusMessage(%q) = %q, want %q", tt.slot, got, tt.want)
			}
		})
	}
}

func TestPollReportShape(t *testing.T) {
	p := PollReport()
	if len(p) != ReportSize {
		t.Fatalf("poll size %d, want %d", len(p), ReportSize)
	}
	if p[0] != '0' {
		t.Errorf("poll byte[0]=0x%02x, want '0' (0x30)", p[0])
	}
}

func TestSampleReportLayout(t *testing.T) {
	for seg := byte(1); seg <= 5; seg++ {
		r := SampleReport(seg)
		if len(r) != ReportSize {
			t.Errorf("seg %d: size %d, want %d", seg, len(r), ReportSize)
		}
		want := byte('0') + seg
		if r[0] != want {
			t.Errorf("seg %d: byte[0]=0x%02x, want 0x%02x ('%c')", seg, r[0], want, want)
		}
		for i := 1; i < ReportSize; i++ {
			if r[i] != 0 {
				t.Errorf("seg %d: byte[%d]=0x%02x, want 0 (entire payload after byte 0 must be zero)", seg, i, r[i])
				break
			}
		}
	}
}

func TestSampleReportRejectsOutOfRange(t *testing.T) {
	for _, seg := range []byte{0, 6, 7, 255} {
		seg := seg
		t.Run(fmt.Sprintf("seg=%d", seg), func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("seg %d: expected panic, got none", seg)
				}
			}()
			_ = SampleReport(seg)
		})
	}
}

func TestVerbAvailableInState(t *testing.T) {
	auto := &Snapshot{AutoChannel: true, Channel: 1}
	tests := []struct {
		name      string
		verb      string
		snap      *Snapshot
		wantBlock bool
	}{
		{"nil snap lets anything through", "range_step", nil, false},
		{"range_step OK in manual channel", "range_step",
			&Snapshot{AutoChannel: false, Channel: 2}, false},
		{"range_step blocked in CH Auto", "range_step", auto, true},
		{"alarm_toggle blocked in CH Auto", "alarm_toggle", auto, true},
		{"alarm_toggle OK in manual channel", "alarm_toggle",
			&Snapshot{AutoChannel: false, Channel: 3}, false},
		{"channel_step allowed in CH Auto", "channel_step", auto, false},
		{"mode_step allowed in CH Auto", "mode_step", auto, false},
		{"peak_toggle allowed in CH Auto", "peak_toggle", auto, false},
		{"setup allowed in CH Auto", "setup", auto, false},
		{"freeze allowed in CH Auto", "freeze", auto, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VerbAvailableInState(tt.verb, tt.snap)
			if tt.wantBlock && !strings.Contains(got, "CH Auto") {
				t.Errorf("expected a CH Auto block reason, got %q", got)
			}
			if !tt.wantBlock && got != "" {
				t.Errorf("expected no block, got %q", got)
			}
		})
	}
}

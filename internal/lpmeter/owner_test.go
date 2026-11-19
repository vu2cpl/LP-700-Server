package lpmeter

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestIsCommandEcho(t *testing.T) {
	tests := []struct {
		name string
		make func() []byte
		want bool
	}{
		{
			name: "true: cmd '1' echo (byte 0 = '1', rest zero)",
			make: func() []byte { r := make([]byte, ReportSize); r[0] = '1'; return r },
			want: true,
		},
		{
			name: "true: cmd '6' echo",
			make: func() []byte { r := make([]byte, ReportSize); r[0] = '6'; return r },
			want: true,
		},
		{
			name: "true: cmd '?' echo (high end of cmd-char range)",
			make: func() []byte { r := make([]byte, ReportSize); r[0] = '?'; return r },
			want: true,
		},
		{
			name: "false: byte 0 in cmd-char range but byte N != 0",
			make: func() []byte {
				r := make([]byte, ReportSize)
				r[0] = '1'
				r[7] = 0xff
				return r
			},
			want: false,
		},
		{
			name: "false: real telemetry — byte 0 = 0 (typical)",
			make: func() []byte {
				r := make([]byte, ReportSize)
				r[3] = 1 // top_mode
				r[6] = 5 // range
				return r
			},
			want: false,
		},
		{
			name: "false: sample frame — byte 0 = 0x97 (151, outside cmd-char range)",
			make: func() []byte {
				r := make([]byte, ReportSize)
				for i := range r {
					r[i] = 0x97
				}
				return r
			},
			want: false,
		},
		{
			name: "false: empty/zero frame (byte 0 = 0, not in cmd range)",
			make: func() []byte { return make([]byte, ReportSize) },
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCommandEcho(tt.make())
			if got != tt.want {
				t.Errorf("isCommandEcho: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsLikelyTelemetry(t *testing.T) {
	// Valid telemetry frame: peak_power >= avg_power, all header
	// bytes in range. Use buildSyntheticFrame and patch peak > avg.
	valid := buildSyntheticFrame(Snapshot{
		Channel: 2, AutoChannel: false,
		Range:        "100W",
		TopMode:      "power_swr",
		PeakMode:     "peak_hold",
		AlarmEnabled: true,
		PowerAvgW:    100, // raw 500
		PowerPeakW:   140, // raw 700
	})
	if !isLikelyTelemetry(valid) {
		t.Error("valid telemetry frame should pass isLikelyTelemetry")
	}

	// Sample frame: 64 bytes of envelope amplitude (e.g. 0x97 = 151).
	// byte 3 (top_mode) = 151 > 3 → fails.
	sample := make([]byte, ReportSize)
	for i := range sample {
		sample[i] = 0x97
	}
	if isLikelyTelemetry(sample) {
		t.Error("uniform-151 sample frame must fail isLikelyTelemetry (byte 3 out of range)")
	}

	// Spectrum-shaped sample with byte 3 in range by chance (e.g.
	// byte 3 = 2) but byte 4 out of range.
	spec := make([]byte, ReportSize)
	for i := range spec {
		spec[i] = 0x42
	}
	spec[3] = 2 // looks like top_mode "spectrum"
	if isLikelyTelemetry(spec) {
		t.Error("sample frame with single in-range header byte must still fail (others out of range)")
	}

	// All-zero frame: what the meter returns for a waveform sample
	// segment with no carrier. Every byte-range check accepts it, and
	// neither the echo filter (byte 0 is not a cmd char) nor Decode
	// rejects it, so it used to decode as power_swr / CH Auto / 5W and
	// make top_mode flap every frame on the waveform page (bench,
	// 2026-11-19). It must route as a sample segment instead.
	zero := make([]byte, ReportSize)
	if isLikelyTelemetry(zero) {
		t.Error("all-zero frame (idle sample segment) must fail isLikelyTelemetry")
	}
	if isCommandEcho(zero) {
		t.Error("all-zero frame is not a command echo (byte 0 is not a cmd char)")
	}

	// Real telemetry captured on the waveform page (2026-11-19, LP-700,
	// LCD on CH 4, Power/SWR in CH Auto): top_mode 1, byte 4 = 0,
	// byte 5 = 4, range 11 (auto), SWR raw 999. Must still pass.
	captured, err := hex.DecodeString("0000030100040b010000040a0000090000000102040000000000002e0130000a0000000000e70000e37b06b784" + strings.Repeat("00", 19))
	if err != nil {
		t.Fatal(err)
	}
	if !isLikelyTelemetry(captured) {
		t.Error("captured waveform-page telemetry frame should pass isLikelyTelemetry")
	}
	if snap, err := Decode(captured); err != nil || snap.TopMode != "waveform" || snap.AutoChannel || snap.Channel != 4 || snap.Range != "auto" {
		t.Errorf("captured frame decoded as %+v, err %v; want waveform, CH 4 (no Auto on this page), range auto", snap, err)
	}

	// Wrong size: short frame.
	if isLikelyTelemetry(make([]byte, ReportSize-1)) {
		t.Error("short frame must fail isLikelyTelemetry")
	}

	// Spectrum cmd '5' tail with sparse small bytes: all header
	// invariants pass by accident, but byte 7 (alarm-DISABLED) is
	// out of {0,1} and/or peak < avg. This is the actual leak path
	// observed in production 2026-05-16 (14 peak<avg incidents in 20s
	// before the byte-7 + peak/avg checks were added).
	leak := make([]byte, ReportSize)
	leak[3] = 2 // top_mode in range
	leak[4] = 1 // channel in range
	leak[5] = 1 // channel-auto in range
	leak[6] = 4 // range in range
	leak[7] = 3 // alarm DISABLED — out of {0,1}
	leak[8] = 1 // peak-mode in range
	if isLikelyTelemetry(leak) {
		t.Error("sample frame with byte 7 = 3 must fail isLikelyTelemetry (alarm flag is 0/1 only)")
	}

	// Same shape but byte 7 happens to be 0 or 1. Then peak<avg
	// catches it.
	peakLessAvg := make([]byte, ReportSize)
	peakLessAvg[3], peakLessAvg[4], peakLessAvg[5] = 2, 1, 1
	peakLessAvg[6] = 4
	peakLessAvg[7] = 1
	peakLessAvg[8] = 1
	// peak (bytes 23-24) = 0x0001 = 1; avg (bytes 25-26) = 0x0500 = 1280
	peakLessAvg[23] = 0x00
	peakLessAvg[24] = 0x01
	peakLessAvg[25] = 0x05
	peakLessAvg[26] = 0x00
	if isLikelyTelemetry(peakLessAvg) {
		t.Error("frame with peak (1) < avg (1280) must fail isLikelyTelemetry")
	}

	// All header bytes in range, peak >= avg, but SWR raw above the
	// physical sanity cap (raw 1000 = SWR 10.0). This is the leak
	// path that produced swr>5 reports on 2026-05-16.
	swrLeak := make([]byte, ReportSize)
	swrLeak[2] = 0x08  // SWR hi
	swrLeak[37] = 0x00 // SWR lo → raw = 0x0800 = 2048 → SWR 20.48
	swrLeak[3], swrLeak[4], swrLeak[5] = 0, 1, 0
	swrLeak[6] = 4
	swrLeak[7] = 1
	swrLeak[8] = 0
	swrLeak[23], swrLeak[24] = 0x00, 0x05 // peak raw 5
	swrLeak[25], swrLeak[26] = 0x00, 0x05 // avg raw 5 (peak == avg ok)
	if isLikelyTelemetry(swrLeak) {
		t.Error("frame with SWR raw > 1000 must fail isLikelyTelemetry")
	}
}

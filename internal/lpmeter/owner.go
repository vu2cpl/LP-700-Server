package lpmeter

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Backend tags the source of telemetry (helpful for the web client to
// surface "you are looking at simulated data").
type Backend string

const (
	BackendHID       Backend = "hid"
	BackendSimulator Backend = "simulator"
)

// Source is anything that produces snapshots and accepts named control
// verbs. Both the real HID backend and the simulator implement it; the
// hub doesn't care which is wired up.
type Source interface {
	// Run blocks until ctx is cancelled. Errors during the loop are
	// logged; the implementation is expected to self-recover (HID
	// reconnect on disconnect; simulator never errors).
	Run(ctx context.Context)
	// Submit queues a control verb for the next interleave slot.
	// Returns false if the queue is full or the verb is unknown so
	// callers can NACK.
	Submit(verb string, value int) bool
	// Backend identifies which implementation is running.
	Backend() Backend
}

// HIDOwner is the single goroutine that holds the LP-500/700 HID handle
// and arbitrates reads (poll responses) and writes (control commands)
// so they never collide.
type HIDOwner struct {
	vendorID    uint16
	productID   uint16
	pollEvery   time.Duration
	commands    chan command
	out         chan<- Snapshot
	scopeOut    chan<- ScopeFrame    // nil → scope assembly disabled
	spectrumOut chan<- SpectrumFrame // nil → spectrum assembly disabled
	logger      *slog.Logger
}

type command struct {
	verb  string
	value int
}

// NewHIDOwner builds an owner that will open an LP-500/700 by VID/PID
// (when both are non-zero) or by Product-string match ("LP-500" /
// "LP-700") otherwise. Writes snapshots to `out`; if non-nil, also
// assembles scope/spectrum buffers when the meter is on the matching
// LCD page and emits them on `scopeOut` / `spectrumOut`.
func NewHIDOwner(vid, pid uint16, pollEvery time.Duration, out chan<- Snapshot, scopeOut chan<- ScopeFrame, spectrumOut chan<- SpectrumFrame, logger *slog.Logger) *HIDOwner {
	return &HIDOwner{
		vendorID:    vid,
		productID:   pid,
		pollEvery:   pollEvery,
		commands:    make(chan command, 16),
		out:         out,
		scopeOut:    scopeOut,
		spectrumOut: spectrumOut,
		logger:      logger,
	}
}

func (o *HIDOwner) Backend() Backend { return BackendHID }

func (o *HIDOwner) Submit(verb string, value int) bool {
	if !KnownVerbs[verb] {
		return false
	}
	select {
	case o.commands <- command{verb, value}:
		return true
	default:
		return false
	}
}

// Run loops forever, opening the device and reading IN reports, reopening
// on error with exponential backoff. Returns only when ctx is cancelled.
func (o *HIDOwner) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		err := o.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			o.logger.Warn("hid loop ended", "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (o *HIDOwner) runOnce(ctx context.Context) error {
	info, err := o.findDevice()
	if err != nil {
		return fmt.Errorf("find device: %w", err)
	}
	dev, err := openHID(info)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer dev.Close()
	o.logger.Info("hid open",
		"path", info.Path,
		"vendor_id", fmt.Sprintf("0x%04x", info.VendorID),
		"product_id", fmt.Sprintf("0x%04x", info.ProductID),
		"product", info.Product,
		"manufacturer", info.Manufacturer)

	// Reads block in a goroutine so the main loop can still drain
	// commands even when the device is silent. The read goroutine
	// signals the main loop to bail by sending its error.
	frames := make(chan []byte, 4)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, ReportSize)
		for {
			n, err := dev.Read(buf)
			if err != nil {
				readErr <- err
				return
			}
			if n != ReportSize {
				// hidraw on Linux can return shorter reads
				// when the device sends a smaller report than
				// our buffer (very unusual for vendor HIDs).
				// Skip rather than mis-parse.
				continue
			}
			frame := make([]byte, ReportSize)
			copy(frame, buf)
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()

	// pollTicker drives both the active poll (cmd '0') and the drain
	// of any queued control verbs from clients. The exact cmd that
	// fires on each tick depends on the meter's current top_mode:
	//   - power_swr / setup: cmd '0' every tick, cmd '6' every Nth
	//     tick (refreshes the ASCII status slot at bytes 40..63)
	//   - waveform / spectrum: 6-tick cycle '0' '1' '2' '3' '4' '5',
	//     so each tick cycle yields one telemetry frame plus the 5
	//     segments that assemble into one ScopeFrame or SpectrumFrame
	//     (~4 Hz scope/spec rate at the default 40 ms poll cadence,
	//     ~4 Hz telemetry rate during scope/spec mode)
	const statusEveryN = 10
	const sampleCycleLen = 6
	pollTicker := time.NewTicker(o.pollEvery)
	defer pollTicker.Stop()
	tickN := 0

	// Sticky status message: only cmd-'6' responses carry it; carry it
	// forward across plain cmd-'0' responses so clients see a stable
	// value rather than text-then-blank flicker.
	var lastStatus string

	// Track meter state from the last decoded telemetry frame. Used
	// by the tick handler to decide whether to interleave sample cmds,
	// and by the sample-frame emitter to label the assembled buffer.
	var lastTopMode string
	var lastChannel int
	var lastAutoCh bool

	// Frame routing is by SHAPE, not by OUT-write order. An earlier
	// implementation matched IN frames to OUT cmds via a FIFO, but
	// that approach desyncs on any single missed event (stale kernel-
	// buffered frame at HID open, an unsolicited firmware frame on
	// mode change) and the misalignment then cascades. With shape-
	// based routing the loop is self-correcting: each frame is
	// classified on its own merits and no per-write state persists
	// across frames.
	//
	// Three frame classes:
	//   1. ECHO     — byte[0] in cmd-char range AND bytes 1..63 zero.
	//                 Firmware refused the OUT (wrong LCD page or no-op
	//                 in current state). Drop.
	//   2. TELEMETRY — non-echo, not all-zero, AND satisfies tight
	//                  byte-range invariants (byte 3 ≤ 3, byte 4 ≤ 4,
	//                  etc.). Probability a random sample frame passes
	//                  all five invariants ≈ 10⁻¹⁰. Run through Decode.
	//   3. SAMPLE   — non-echo, fails telemetry invariants. In
	//                  waveform/spectrum mode this is the next segment
	//                  of the 5×64-byte buffer; assemble in arrival
	//                  order (the 1:1 firmware response keeps order
	//                  aligned with our 6-tick cycle). An idle segment
	//                  (no carrier) is 64 zero bytes and lands here too.

	// Scope / spectrum buffer assembly state. sampleSegIdx advances
	// with each non-telemetry, non-echo frame received while in a
	// sample-bearing mode; it resets to 0 on every telemetry frame
	// (start of a fresh 6-tick cycle) and on every mode change.
	var scopeBuf [SampleBufferSize]byte
	var spectrumBuf [SampleBufferSize]byte
	sampleSegIdx := 0
	resetSampleState := func() {
		sampleSegIdx = 0
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return fmt.Errorf("hid read: %w", err)
		case frame := <-frames:
			// Echo frame? Firmware refused the OUT. Drop & reset any
			// in-progress sample buffer.
			if isCommandEcho(frame) {
				o.logger.Debug("cmd echo (firmware refused)", "cmd", string(frame[0]), "top_mode", lastTopMode)
				resetSampleState()
				continue
			}
			// Telemetry-shaped? Tight byte-range invariants (~10⁻¹⁰
			// false-positive rate on random sample data). Decode and
			// broadcast.
			if isLikelyTelemetry(frame) {
				snap, err := Decode(frame)
				if err != nil {
					if IsSkippable(err) {
						continue
					}
					o.logger.Debug("decode error", "err", err, "raw", fmt.Sprintf("%x", frame))
					continue
				}
				// Bytes 40..63 only carry an ASCII status message after
				// cmd '6'. After cmd '0' that slot holds bargraph /
				// pwr-mult bytes and extractStatusMessage returns "".
				// Carry the last non-empty message forward across plain
				// telemetry frames so clients see stable text.
				if snap.StatusMessage == "" {
					snap.StatusMessage = lastStatus
				} else {
					lastStatus = snap.StatusMessage
				}
				// A telemetry frame marks the start of a fresh 6-tick
				// cycle. Reset the sample-segment counter so the next
				// 5 non-telemetry frames assemble seg 1..5 in order.
				sampleSegIdx = 0
				// Top-mode change invalidates any in-progress assembly.
				if snap.TopMode != lastTopMode {
					resetSampleState()
				}
				lastTopMode = snap.TopMode
				lastChannel = snap.Channel
				lastAutoCh = snap.AutoChannel
				o.logger.Debug("frame",
					"channel", snap.Channel,
					"auto_channel", snap.AutoChannel,
					"power_avg_w", snap.PowerAvgW,
					"power_peak_w", snap.PowerPeakW,
					"peak_hold_w", snap.PeakHoldW,
					"peak_mode", snap.PeakMode,
					"swr", snap.SWR,
					"range", snap.Range,
					"alarm_enabled", snap.AlarmEnabled,
					"status", snap.StatusMessage,
					"raw", fmt.Sprintf("%x", frame))
				select {
				case o.out <- snap:
				case <-ctx.Done():
					return nil
				default:
					// Hub is slow; drop this sample. Next IN report
					// will replace it.
				}
				continue
			}
			// Sample frame. Route into the appropriate buffer based
			// on the last-known top_mode. The firmware delivers
			// segments 1..5 in cmd-order, 1:1 with our writes, so
			// arrival order matches segment index — no need to know
			// the originating cmd byte.
			if lastTopMode != "waveform" && lastTopMode != "spectrum" {
				// Not on a sample-bearing page; drop (likely a stale
				// frame from a recent mode transition).
				continue
			}
			if sampleSegIdx >= 5 {
				// More sample frames than the cycle should produce;
				// drop. Will realign on the next telemetry frame.
				o.logger.Debug("extra sample frame past seg 5", "top_mode", lastTopMode)
				continue
			}
			segIdx := sampleSegIdx
			sampleSegIdx++
			switch lastTopMode {
			case "waveform":
				copy(scopeBuf[segIdx*64:(segIdx+1)*64], frame)
				o.logger.Debug("scope segment received", "seg", segIdx+1)
				if sampleSegIdx == 5 {
					o.emitScope(scopeBuf[:], lastChannel, lastAutoCh, ctx)
				}
			case "spectrum":
				copy(spectrumBuf[segIdx*64:(segIdx+1)*64], frame)
				o.logger.Debug("spectrum segment received", "seg", segIdx+1)
				if sampleSegIdx == 5 {
					o.emitSpectrum(spectrumBuf[:], lastChannel, lastAutoCh, ctx)
				}
			}
		case <-pollTicker.C:
			if err := o.drainCommands(dev); err != nil {
				return err
			}
			tickN++
			var payload []byte
			switch lastTopMode {
			case "waveform", "spectrum":
				// 6-tick cycle: phase 0 = telemetry poll, 1..5 = sample segments.
				phase := tickN % sampleCycleLen
				if phase == 0 {
					payload = PollReport()
				} else {
					payload = SampleReport(byte(phase))
				}
			default:
				if tickN%statusEveryN == 0 {
					payload = StatusReport()
				} else {
					payload = PollReport()
				}
			}
			if err := writeReport(dev, payload); err != nil {
				return fmt.Errorf("poll: %w", err)
			}
		}
	}
}

// drainCommands writes any queued control verbs as 64-byte OUT reports
// with a short post-write settle so the meter can fully process each
// before the next.
func (o *HIDOwner) drainCommands(dev hidDevice) error {
	const postWriteSettle = 50 * time.Millisecond
	for {
		select {
		case cmd := <-o.commands:
			out, err := EncodeCommand(cmd.verb, cmd.value)
			if err != nil {
				o.logger.Warn("encode command", "verb", cmd.verb, "err", err)
				continue
			}
			if err := writeReport(dev, out); err != nil {
				return fmt.Errorf("hid write %s: %w", cmd.verb, err)
			}
			o.logger.Debug("sent command", "verb", cmd.verb, "value", cmd.value)
			time.Sleep(postWriteSettle)
		default:
			return nil
		}
	}
}

// writeReport sends a 64-byte HID OUT report. The Linux hidraw driver
// expects the first byte of the buffer to be the Report ID — the
// LP-500/700's mikroE HID stack uses no Report IDs, so we prepend a
// 0x00 to make a 65-byte buffer where bytes [1..] are the 64-byte
// payload.
func writeReport(dev hidDevice, payload []byte) error {
	buf := make([]byte, 1+len(payload))
	copy(buf[1:], payload)
	_, err := dev.Write(buf)
	return err
}

func (o *HIDOwner) findDevice() (hidDeviceInfo, error) {
	devices, err := enumerateHID()
	if err != nil {
		return hidDeviceInfo{}, fmt.Errorf("enumerate: %w", err)
	}
	if len(devices) == 0 {
		return hidDeviceInfo{}, errors.New("no HID devices enumerable (try `lp700-server probe -list`)")
	}
	// Match by VID:PID first when both are configured.
	if o.vendorID != 0 && o.productID != 0 {
		for _, d := range devices {
			if d.VendorID == o.vendorID && d.ProductID == o.productID {
				return d, nil
			}
		}
	}
	for _, d := range devices {
		if isLPMeter(d.Product) {
			return d, nil
		}
	}
	return hidDeviceInfo{}, errors.New("no LP-500/LP-700 found in HID enumeration")
}

func isLPMeter(product string) bool {
	p := strings.ToUpper(product)
	return strings.Contains(p, "LP-500") || strings.Contains(p, "LP-700") ||
		strings.Contains(p, "LP500") || strings.Contains(p, "LP700")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isCommandEcho reports whether a 64-byte IN frame is the firmware's
// echo of an OUT command write (byte[0] in the cmd-char range '0'..'?'
// and every other byte zero). Real telemetry never matches.
func isCommandEcho(frame []byte) bool {
	if len(frame) == 0 || frame[0] < '0' || frame[0] > '?' {
		return false
	}
	for i := 1; i < len(frame); i++ {
		if frame[i] != 0 {
			return false
		}
	}
	return true
}

// isAllZero reports whether every byte of the frame is zero.
func isAllZero(frame []byte) bool {
	for _, b := range frame {
		if b != 0 {
			return false
		}
	}
	return true
}

// isLikelyTelemetry reports whether a 64-byte IN frame's structure
// matches a real telemetry response from cmd '0' or '6': specifically
// that the header-byte ranges that the Decode function later relies on
// are all within their valid intervals. Sample frames (from cmds
// '1'..'5' in waveform/spectrum mode) place arbitrary 8-bit sample
// data at these offsets, so the chance of all five passing on a sample
// frame is the product of each range's relative width — under 10⁻¹⁰
// for random data, slightly higher in pathological correlated cases
// but never observed empirically on the LP-700 firmware.
//
// This is the key defense against sample frames leaking into the
// telemetry decode path. It is intentionally STRICTER than what
// Decode itself enforces, because Decode would also accept some
// sample frames (those that happen to have all 5 header bytes in
// range with garbage payload values) and broadcast garbage Snapshots.
//
// An all-zero frame is never telemetry. It is what the meter returns
// for a waveform sample segment when there is no carrier, and every
// byte-range check below accepts it (0 is in range everywhere, and
// peak 0 ≥ avg 0). Decoded, it reads as power_swr / CH Auto / 5W /
// SWR 1.00, so before this check the run loop flipped lastTopMode to
// power_swr on every idle segment, polled cmd '0' again on the next
// tick, saw waveform, and never completed a sample cycle: clients saw
// top_mode / channel / range / SWR flap every frame and no scope
// frames (bench, 2026-11-19, v0.2.6). Real telemetry carries non-zero
// bytes (the captured waveform-page frame has the full-scale bytes
// 30-31 = 0x000a and the bargraph scaling at 27-29 set), and an
// all-zero frame would decode to nothing meaningful anyway, so
// rejecting it costs at most one dropped frame.
func isLikelyTelemetry(frame []byte) bool {
	if len(frame) != ReportSize {
		return false
	}
	if isAllZero(frame) {
		return false
	}
	if frame[OffsetTopMode] > 3 {
		return false
	}
	if frame[OffsetChannel] > 4 {
		return false
	}
	if frame[OffsetChannelAuto] > 4 {
		return false
	}
	if int(frame[OffsetRange]) >= len(rangeNames) {
		return false
	}
	// Byte 7 is alarm-DISABLED, a flag byte: 0 or 1 only.
	if frame[OffsetAlarm] > 1 {
		return false
	}
	if int(frame[OffsetPeakAvg]) >= len(peakModeNames) {
		return false
	}
	// Power coherence: in real telemetry frames the firmware
	// guarantees peak_power >= avg_power (peak is a max-hold over a
	// short window, avg is a rolling integration over the same or
	// longer window). Sample frames place arbitrary u8 sample values
	// at these offsets and routinely violate this invariant — the
	// remaining "peak < avg" leakage past the byte-range checks falls
	// into this filter.
	rawPeak := binary.BigEndian.Uint16(frame[OffsetPeakPwrHi : OffsetPeakPwrHi+2])
	rawAvg := binary.BigEndian.Uint16(frame[OffsetAvgPwrHi : OffsetAvgPwrHi+2])
	if rawPeak < rawAvg {
		return false
	}
	// SWR sanity: raw SWR is a 16-bit fixed-point /100 value. The
	// meter caps physically meaningful SWR around 5.0 (raw 500); the
	// internal floor is 1.0 (raw 100). Values above raw ~1000 (SWR
	// 10) are unphysical for any real coupler/load and indicate
	// sample data has slipped into bytes 2 / 37. Cap at raw 1000 to
	// catch the residual leak.
	rawSWR := uint16(frame[OffsetSWRHi])<<8 | uint16(frame[OffsetSWRLo])
	if rawSWR > 1000 {
		return false
	}
	return true
}

// emitScope copies `buf` into a fresh ScopeFrame and sends it on the
// scope channel non-blocking. If the channel is nil (assembly
// disabled) or full (slow consumer), the frame is dropped — a fresh
// one will be assembled on the next 6-tick cycle.
func (o *HIDOwner) emitScope(buf []byte, channel int, autoCh bool, ctx context.Context) {
	if o.scopeOut == nil {
		return
	}
	// Defensive guard. The waveform / spectrum pages have their own
	// CH 1..4 setting with no Auto, and Decode reports it that way
	// (byte 5, AutoChannel false), so on a real meter this only trips
	// on a frame whose channel bytes are out of range. It used to trip
	// on every frame while Power/SWR was in CH Auto, because byte 4
	// was read as Auto on these pages too — see Decode.
	if autoCh || channel < 1 || channel > 4 {
		o.logger.Debug("scope frame suppressed (invalid channel/auto state)", "channel", channel, "auto_channel", autoCh)
		return
	}
	samples := make(SampleBytes, len(buf))
	copy(samples, buf)
	frame := ScopeFrame{
		Timestamp:   time.Now().UTC(),
		TopMode:     "waveform",
		Channel:     channel,
		AutoChannel: autoCh,
		Samples:     samples,
	}
	select {
	case o.scopeOut <- frame:
		o.logger.Debug("emit scope", "channel", channel, "samples_min_max", minMax(samples))
	case <-ctx.Done():
	default:
		o.logger.Debug("scope buffer dropped (hub slow)")
	}
}

// emitSpectrum is the spectrum-buffer equivalent of emitScope.
func (o *HIDOwner) emitSpectrum(buf []byte, channel int, autoCh bool, ctx context.Context) {
	if o.spectrumOut == nil {
		return
	}
	if autoCh || channel < 1 || channel > 4 {
		o.logger.Debug("spectrum frame suppressed (invalid channel/auto state)", "channel", channel, "auto_channel", autoCh)
		return
	}
	bins := make(SampleBytes, len(buf))
	copy(bins, buf)
	frame := SpectrumFrame{
		Timestamp:   time.Now().UTC(),
		TopMode:     "spectrum",
		Channel:     channel,
		AutoChannel: autoCh,
		Bins:        bins,
	}
	select {
	case o.spectrumOut <- frame:
		o.logger.Debug("emit spectrum", "channel", channel, "bins_min_max", minMax(bins))
	case <-ctx.Done():
	default:
		o.logger.Debug("spectrum buffer dropped (hub slow)")
	}
}

// minMax is a tiny diagnostic helper for the emit-debug log: returns a
// "min..max" string summarising a sample buffer.
func minMax(b SampleBytes) string {
	if len(b) == 0 {
		return "(empty)"
	}
	mn, mx := b[0], b[0]
	for _, v := range b {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	return fmt.Sprintf("%d..%d", mn, mx)
}

package lpmeter

import (
	"context"
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

	// pendingCmds is a FIFO of byte[0] values from OUT writes whose
	// IN response hasn't arrived yet. Matching writes 1:1 with reads
	// is how we tell a telemetry frame (after '0' / '6') from a
	// sample-segment frame (after '1'..'5') — the latter contains 64
	// bytes of raw envelope/FFT data that does NOT decode as
	// telemetry. The firmware emits exactly one IN report per OUT
	// report (verified by inspection of the probe captures), so a
	// simple FIFO is sufficient.
	pendingCmds := make([]byte, 0, 16)
	pushCmd := func(c byte) { pendingCmds = append(pendingCmds, c) }
	popCmd := func() byte {
		if len(pendingCmds) == 0 {
			return 0 // unexpected; caller defaults to telemetry decode
		}
		c := pendingCmds[0]
		pendingCmds = pendingCmds[1:]
		return c
	}
	writeOut := func(payload []byte) error {
		pushCmd(payload[0])
		return writeReport(dev, payload)
	}

	// Scope / spectrum buffer assembly state. Filled segment-by-
	// segment from cmd '1'..'5' responses; emitted on completion.
	var scopeBuf [SampleBufferSize]byte
	var spectrumBuf [SampleBufferSize]byte
	var scopeFilled [5]bool
	var spectrumFilled [5]bool
	resetSampleState := func() {
		for i := range scopeFilled {
			scopeFilled[i] = false
		}
		for i := range spectrumFilled {
			spectrumFilled[i] = false
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return fmt.Errorf("hid read: %w", err)
		case frame := <-frames:
			cmd := popCmd()
			if cmd >= '1' && cmd <= '5' {
				// Response to a sample-segment write. Two
				// possibilities:
				//   (a) Meter is on the matching LCD page →
				//       64 bytes of envelope/FFT data
				//   (b) Meter is NOT on the matching page →
				//       echo frame (byte[0]=cmd, rest zero);
				//       the firmware refuses sample reads off
				//       the corresponding page
				if isCommandEcho(frame) {
					o.logger.Debug("sample echo (firmware refused)", "cmd", string(cmd), "top_mode", lastTopMode)
					resetSampleState()
					continue
				}
				segIdx := int(cmd - '1')
				switch lastTopMode {
				case "waveform":
					copy(scopeBuf[segIdx*64:(segIdx+1)*64], frame)
					scopeFilled[segIdx] = true
					o.logger.Debug("scope segment received", "seg", segIdx+1, "filled", scopeFilled)
					if allFilled(scopeFilled[:]) {
						o.emitScope(scopeBuf[:], lastChannel, lastAutoCh, ctx)
						for i := range scopeFilled {
							scopeFilled[i] = false
						}
					}
				case "spectrum":
					copy(spectrumBuf[segIdx*64:(segIdx+1)*64], frame)
					spectrumFilled[segIdx] = true
					o.logger.Debug("spectrum segment received", "seg", segIdx+1, "filled", spectrumFilled)
					if allFilled(spectrumFilled[:]) {
						o.emitSpectrum(spectrumBuf[:], lastChannel, lastAutoCh, ctx)
						for i := range spectrumFilled {
							spectrumFilled[i] = false
						}
					}
				default:
					o.logger.Debug("sample frame in unexpected top_mode", "cmd", string(cmd), "top_mode", lastTopMode)
				}
				continue
			}
			// Treat all other frames as telemetry-shaped (response
			// to '0' / '6' / unknown). Decode handles cmd echoes
			// and short frames internally.
			snap, err := Decode(frame)
			if err != nil {
				if IsSkippable(err) {
					continue
				}
				o.logger.Debug("decode error", "err", err, "raw", fmt.Sprintf("%x", frame))
				continue
			}
			// Bytes 40..63 only carry an ASCII status message after a
			// cmd '6' poll. After cmd '0' that slot holds binary
			// scope/spec data and Decode returns "". Carry the last
			// non-empty message forward so it stays visible to clients
			// between cmd-'6' refreshes.
			if snap.StatusMessage == "" {
				snap.StatusMessage = lastStatus
			} else {
				lastStatus = snap.StatusMessage
			}
			// Top-mode change invalidates any in-progress sample
			// assembly (segments from waveform shouldn't be mixed
			// with segments from spectrum).
			if snap.TopMode != lastTopMode {
				resetSampleState()
			}
			lastTopMode = snap.TopMode
			lastChannel = snap.Channel
			lastAutoCh = snap.AutoChannel

			// Full-frame hex in the debug log lets us see whether the
			// firmware interleaves multiple IN-report types (e.g. Power/
			// SWR vs status), which we'd otherwise conflate. Cheap on a
			// quiet journal because the level defaults to error.
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
				// Hub is slow; drop this sample. Next IN report will replace it.
			}
		case <-pollTicker.C:
			if err := o.drainCommandsVia(writeOut); err != nil {
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
			if err := writeOut(payload); err != nil {
				return fmt.Errorf("poll: %w", err)
			}
		}
	}
}

// drainCommandsVia writes any queued control verbs through `write`,
// which is the runOnce closure that also tracks pending OUT cmds for
// IN-frame attribution. A short post-write settle lets the meter
// fully process each verb before the next OUT lands.
func (o *HIDOwner) drainCommandsVia(write func([]byte) error) error {
	const postWriteSettle = 50 * time.Millisecond
	for {
		select {
		case cmd := <-o.commands:
			out, err := EncodeCommand(cmd.verb, cmd.value)
			if err != nil {
				o.logger.Warn("encode command", "verb", cmd.verb, "err", err)
				continue
			}
			if err := write(out); err != nil {
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

// allFilled reports whether every element of `flags` is true.
func allFilled(flags []bool) bool {
	for _, f := range flags {
		if !f {
			return false
		}
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

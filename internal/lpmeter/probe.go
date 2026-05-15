package lpmeter

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ProbeMode picks one of three diagnostic actions the binary's `probe`
// subcommand can perform.
type ProbeMode string

const (
	ProbeList    ProbeMode = "list"    // enumerate every HID
	ProbeDump    ProbeMode = "dump"    // print every IN report from the matched LP-500/700
	ProbeCapture ProbeMode = "capture" // write a fixture file
	ProbeSamples ProbeMode = "samples" // cycle OUT cmds '1'..'5' and dump the secondary slot
)

// ProbeOptions wraps the CLI flags driving the probe subcommand.
type ProbeOptions struct {
	Mode      ProbeMode
	OutPath   string        // for ProbeCapture
	Duration  time.Duration // for ProbeCapture; 0 = until ^C
	VendorID  uint16
	ProductID uint16

	// For ProbeSamples:
	FramesPerCmd  int    // IN frames captured per OUT cmd; 0 → default 16
	CycleModes    bool   // mode_step through power_swr / waveform / spectrum and repeat per cmd
	TargetChannel int    // 1..4: channel_step until manual ch matches; 0 = leave as-is
	TargetRange   string // "5W" / "10W" / ... / "10K" / "auto"; "" = leave as-is
}

// RunProbe executes one of the diagnostic modes. Output goes to `w`.
func RunProbe(ctx context.Context, opts ProbeOptions, w io.Writer) error {
	switch opts.Mode {
	case ProbeList:
		return runList(w, opts)
	case ProbeDump:
		return runDump(ctx, w, opts)
	case ProbeCapture:
		return runCapture(ctx, w, opts)
	case ProbeSamples:
		return runSamples(ctx, w, opts)
	default:
		return fmt.Errorf("unknown probe mode %q", opts.Mode)
	}
}

func runList(w io.Writer, opts ProbeOptions) error {
	devs, err := enumerateHID()
	if err != nil {
		return fmt.Errorf("enumerate: %w", err)
	}
	if len(devs) == 0 {
		fmt.Fprintln(w, "No HID devices enumerable. On Linux, ensure /dev/hidraw* exists and the lp700 user can read it (install the udev rule).")
		return nil
	}
	fmt.Fprintf(w, "%-15s  %-15s  %-7s  %-7s  %-30s  %s\n", "marker", "path", "vid", "pid", "product", "manufacturer")
	for _, d := range devs {
		marker := "  "
		if isLPMeter(d.Product) || (opts.VendorID != 0 && opts.ProductID != 0 && d.VendorID == opts.VendorID && d.ProductID == opts.ProductID) {
			marker = "* LP-500/700"
		}
		fmt.Fprintf(w, "%-15s  %-15s  0x%04x   0x%04x   %-30s  %s\n",
			marker, d.Path, d.VendorID, d.ProductID, truncate(d.Product, 30), d.Manufacturer)
	}
	return nil
}

func runDump(ctx context.Context, w io.Writer, opts ProbeOptions) error {
	dev, info, err := openLPMeter(opts)
	if err != nil {
		return err
	}
	defer dev.Close()
	fmt.Fprintf(w, "# Opened %s (%04x:%04x %q manuf=%q)\n", info.Path, info.VendorID, info.ProductID, info.Product, info.Manufacturer)
	fmt.Fprintln(w, "# Each line is one IN report. raw=hex  decoded=human")

	// Drive a poll on a ticker so the meter actually emits frames.
	pollErr := make(chan error, 1)
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writeReport(dev, PollReport()); err != nil {
					pollErr <- err
					return
				}
			}
		}
	}()

	buf := make([]byte, ReportSize)
	for ctx.Err() == nil {
		select {
		case e := <-pollErr:
			return fmt.Errorf("poll: %w", e)
		default:
		}
		n, err := dev.Read(buf)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if n == 0 {
			continue
		}
		printFrame(w, buf[:n])
	}
	return nil
}

func runCapture(ctx context.Context, w io.Writer, opts ProbeOptions) error {
	if opts.OutPath == "" {
		return errors.New("capture requires -capture <path>")
	}
	dev, info, err := openLPMeter(opts)
	if err != nil {
		return err
	}
	defer dev.Close()

	f, err := os.Create(opts.OutPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", opts.OutPath, err)
	}
	defer f.Close()

	deadline := time.Time{}
	if opts.Duration > 0 {
		deadline = time.Now().Add(opts.Duration)
	}

	header := fmt.Sprintf("# LP-500/700 HID capture\n# device: %s %04x:%04x %q\n# started: %s\n# duration: %s\n# format: 64 bytes per frame, raw\n",
		info.Path, info.VendorID, info.ProductID, info.Product, time.Now().Format(time.RFC3339), opts.Duration)
	if _, err := f.WriteString(header); err != nil {
		return err
	}
	fmt.Fprintf(w, "Capturing to %s. Press ^C to stop.\n", opts.OutPath)

	// Drive a poll ticker as in dump.
	pollErr := make(chan error, 1)
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writeReport(dev, PollReport()); err != nil {
					pollErr <- err
					return
				}
			}
		}
	}()

	buf := make([]byte, ReportSize)
	frames := 0
	for ctx.Err() == nil {
		select {
		case e := <-pollErr:
			return fmt.Errorf("poll: %w", e)
		default:
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		n, err := dev.Read(buf)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if n != ReportSize {
			continue
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		frames++
	}
	fmt.Fprintf(w, "Wrote %d frames to %s\n", frames, opts.OutPath)
	return nil
}

func openLPMeter(opts ProbeOptions) (hidDevice, hidDeviceInfo, error) {
	devs, err := enumerateHID()
	if err != nil {
		return nil, hidDeviceInfo{}, fmt.Errorf("enumerate: %w", err)
	}
	if len(devs) == 0 {
		return nil, hidDeviceInfo{}, errors.New("no HID devices enumerable")
	}
	for _, d := range devs {
		if (opts.VendorID != 0 && opts.ProductID != 0 && d.VendorID == opts.VendorID && d.ProductID == opts.ProductID) || isLPMeter(d.Product) {
			dev, err := openHID(d)
			if err != nil {
				return nil, hidDeviceInfo{}, fmt.Errorf("open: %w", err)
			}
			return dev, d, nil
		}
	}
	return nil, hidDeviceInfo{}, errors.New("no LP-500/LP-700 found")
}

func printFrame(w io.Writer, frame []byte) {
	human := "skip"
	if snap, err := Decode(frame); err == nil {
		human = fmt.Sprintf("ch=%d auto=%t avg=%.1fW peak=%.1fW swr=%.2f range=%s",
			snap.Channel, snap.AutoChannel, snap.PowerAvgW, snap.PowerPeakW, snap.SWR, snap.Range)
	} else if IsSkippable(err) {
		human = fmt.Sprintf("non-telemetry frame type=0x%02x", frame[0])
	} else {
		human = "decode error: " + err.Error()
	}
	fmt.Fprintf(w, "raw=%s  decoded=%s\n", hex.EncodeToString(frame), human)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// IsLPMeterProductString is exported so the main package can use the same
// match function when reporting backend selection in the config endpoint.
func IsLPMeterProductString(s string) bool { return isLPMeter(s) }

// runSamples drives OUT commands '1'..'5' and dumps the secondary slot
// (bytes 40..63) of the IN reports that follow each. The Telepost
// DataLogger VB6 source documents '0' as the live-telemetry poll and
// '6' as the status-message refresh; '1'..'5' are mentioned only as
// "scope/spectrum sample buffers" with no field-level documentation.
// This mode is the first step in reverse-engineering those buffers.
//
// For each cmd:
//
//	send the cmd as an OUT report
//	read N follow-up IN reports
//	for each non-echo IN frame: print raw[40:63] hex, signed/unsigned
//	  8-bit, signed/unsigned 16-bit BE/LE views, and the top_mode /
//	  channel header so we can correlate the slot's content with what
//	  the LCD is showing
//
// With -cycle-modes the probe will mode_step the meter through
// power_swr → waveform → spectrum and repeat the per-cmd capture in
// each, then mode_step back to power_swr. The "gotcha" working
// hypothesis is that the firmware only populates the scope slot when
// top_mode == waveform and the spectrum slot when top_mode == spectrum.
func runSamples(ctx context.Context, w io.Writer, opts ProbeOptions) error {
	frames := opts.FramesPerCmd
	if frames <= 0 {
		frames = 16
	}

	dev, info, err := openLPMeter(opts)
	if err != nil {
		return err
	}
	defer dev.Close()
	fmt.Fprintf(w, "# Opened %s (%04x:%04x %q manuf=%q)\n", info.Path, info.VendorID, info.ProductID, info.Product, info.Manufacturer)
	fmt.Fprintln(w, "# Probe: cycle OUT cmds '1'..'5' and dump secondary slot (bytes 40..63)")

	// A read pump in a goroutine so we can flush junk between OUT
	// writes. The probe drives writes synchronously between captures.
	type framePkt struct {
		buf []byte
		err error
	}
	reads := make(chan framePkt, 32)
	readCtx, cancelReads := context.WithCancel(ctx)
	defer cancelReads()
	go func() {
		buf := make([]byte, ReportSize)
		for readCtx.Err() == nil {
			n, err := dev.Read(buf)
			if err != nil {
				select {
				case reads <- framePkt{err: err}:
				case <-readCtx.Done():
				}
				return
			}
			if n != ReportSize {
				continue
			}
			frame := make([]byte, ReportSize)
			copy(frame, buf)
			select {
			case reads <- framePkt{buf: frame}:
			case <-readCtx.Done():
				return
			}
		}
	}()

	// drain consumes any pending reads for `d` so old data doesn't
	// pollute the next capture window.
	drain := func(d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		for {
			select {
			case <-reads:
			case <-t.C:
				return
			case <-ctx.Done():
				return
			}
		}
	}

	// captureOne sends the OUT cmd `n` times at the meter's natural
	// poll cadence (~40 ms) and prints one IN frame per write. The
	// firmware emits one IN report per OUT report; without repeated
	// writes we'd only see the first response and then silence. Each
	// frame is dumped in full (all 64 bytes) since in scope/spec modes
	// the entire report becomes sample data — the bytes-40..63 "slot"
	// abstraction only holds for telemetry/status frames.
	captureOne := func(cmd byte, n int) (*Snapshot, error) {
		out := make([]byte, ReportSize)
		out[0] = cmd
		drain(20 * time.Millisecond) // flush stale
		var lastSnap *Snapshot
		fmt.Fprintf(w, "\n--- cmd '%c' (0x%02x) ----------------------------------------\n", cmd, cmd)
		// Dedup state: when consecutive frames are byte-identical we
		// only print the first and a tally, since 60 dumps of the same
		// 64 bytes drown the interesting transitions in noise.
		var prevFrame []byte
		dupCount := 0
		flushDups := func() {
			if dupCount > 0 {
				fmt.Fprintf(w, "       (^ %d more identical frames)\n", dupCount)
				dupCount = 0
			}
		}
		for i := 0; i < n; i++ {
			if err := writeReport(dev, out); err != nil {
				flushDups()
				return lastSnap, fmt.Errorf("write cmd 0x%02x (#%d): %w", cmd, i, err)
			}
			select {
			case pkt := <-reads:
				if pkt.err != nil {
					flushDups()
					return lastSnap, fmt.Errorf("read: %w", pkt.err)
				}
				if snap, err := Decode(pkt.buf); err == nil {
					lastSnap = &snap
				}
				if prevFrame != nil && bytesEqual(prevFrame, pkt.buf) {
					dupCount++
				} else {
					flushDups()
					formatFullFrame(w, pkt.buf, i)
					prevFrame = make([]byte, ReportSize)
					copy(prevFrame, pkt.buf)
				}
			case <-time.After(150 * time.Millisecond):
				flushDups()
				fmt.Fprintf(w, "  [%2d] (timeout)\n", i)
				prevFrame = nil // reset; next real frame should print
			case <-ctx.Done():
				flushDups()
				return lastSnap, ctx.Err()
			}
			// Pace at ~25 Hz, same cadence the running server uses.
			time.Sleep(40 * time.Millisecond)
		}
		flushDups()
		return lastSnap, nil
	}

	// readCurrentMode polls a couple times to learn the meter's current
	// top_mode before we start cycling.
	readCurrentMode := func() string {
		_ = writeReport(dev, PollReport())
		for i := 0; i < 4; i++ {
			select {
			case pkt := <-reads:
				if pkt.err != nil {
					return ""
				}
				if snap, err := Decode(pkt.buf); err == nil {
					return snap.TopMode
				}
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return ""
			}
		}
		return ""
	}

	// stepToMode mode_steps until we observe `target` in a decoded
	// frame, or we give up after `maxSteps`.
	stepToMode := func(target string, maxSteps int) (string, error) {
		for step := 0; step < maxSteps; step++ {
			cur := readCurrentMode()
			fmt.Fprintf(w, "# observed top_mode=%q (target %q, step %d)\n", cur, target, step)
			if cur == target {
				return cur, nil
			}
			out := make([]byte, ReportSize)
			out[0] = cmdMode
			if err := writeReport(dev, out); err != nil {
				return cur, fmt.Errorf("write mode_step: %w", err)
			}
			time.Sleep(150 * time.Millisecond)
			drain(80 * time.Millisecond)
		}
		return "", fmt.Errorf("could not reach top_mode=%q in %d steps", target, maxSteps)
	}

	// readCurrentSnap polls once and returns a fresh decoded snapshot.
	readCurrentSnap := func() *Snapshot {
		_ = writeReport(dev, PollReport())
		for i := 0; i < 4; i++ {
			select {
			case pkt := <-reads:
				if pkt.err != nil {
					return nil
				}
				if snap, err := Decode(pkt.buf); err == nil {
					return &snap
				}
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return nil
			}
		}
		return nil
	}

	// stepToChannel channel_steps until we observe (AutoChannel=false,
	// Channel=target). Cycle order on this firmware is
	// 1 → 2 → 3 → 4 → auto → 1, so at most 5 steps reach any state.
	stepToChannel := func(target int) error {
		for step := 0; step < 8; step++ {
			snap := readCurrentSnap()
			if snap == nil {
				return errors.New("could not read state for channel-step")
			}
			fmt.Fprintf(w, "# observed channel=%d auto_ch=%t (target ch=%d manual, step %d)\n",
				snap.Channel, snap.AutoChannel, target, step)
			if !snap.AutoChannel && snap.Channel == target {
				return nil
			}
			out := make([]byte, ReportSize)
			out[0] = cmdChannel
			if err := writeReport(dev, out); err != nil {
				return fmt.Errorf("write channel_step: %w", err)
			}
			time.Sleep(150 * time.Millisecond)
			drain(80 * time.Millisecond)
		}
		return fmt.Errorf("could not reach manual channel %d", target)
	}

	// stepToRange range_steps until snap.Range == target. Firmware
	// requires manual channel for F3 to take effect (we already
	// gate this in VerbAvailableInState), so the caller must have
	// called stepToChannel first. Cycle is 5W → 10W → … → 10K →
	// auto → 5W, 12 entries.
	stepToRange := func(target string) error {
		for step := 0; step < 14; step++ {
			snap := readCurrentSnap()
			if snap == nil {
				return errors.New("could not read state for range-step")
			}
			fmt.Fprintf(w, "# observed range=%q (target %q, step %d)\n", snap.Range, target, step)
			if snap.Range == target {
				return nil
			}
			if snap.AutoChannel {
				return errors.New("cannot range_step while in auto-channel; channel_step first")
			}
			out := make([]byte, ReportSize)
			out[0] = cmdRange
			if err := writeReport(dev, out); err != nil {
				return fmt.Errorf("write range_step: %w", err)
			}
			time.Sleep(150 * time.Millisecond)
			drain(80 * time.Millisecond)
		}
		return fmt.Errorf("could not reach range %q", target)
	}

	originalSnap := readCurrentSnap()
	if originalSnap != nil {
		fmt.Fprintf(w, "# Starting state: top_mode=%q channel=%d auto_ch=%t range=%q\n",
			originalSnap.TopMode, originalSnap.Channel, originalSnap.AutoChannel, originalSnap.Range)
	}
	originalMode := ""
	if originalSnap != nil {
		originalMode = originalSnap.TopMode
	}

	// Drive channel + range first so the sample sweep sees a stable
	// scale. Auto-channel can flip between channels mid-trace; auto-
	// range can rescale the buffer's byte-to-watts mapping halfway
	// through; both make analysis harder.
	//
	// IMPORTANT: F2 (channel) and F3 (range) are no-ops when the
	// meter is on the waveform or spectrum LCD page — same context-
	// sensitivity as F3 in auto-channel. If the meter starts in one
	// of those modes, mode_step it to power_swr before adjusting.
	if (opts.TargetChannel > 0 || opts.TargetRange != "") && originalMode != "" && originalMode != "power_swr" {
		fmt.Fprintf(w, "\n# Switching to power_swr to allow F2/F3 control writes\n")
		if _, err := stepToMode("power_swr", 8); err != nil {
			fmt.Fprintf(w, "# WARN: %v — F2/F3 writes may be no-ops\n", err)
		}
		drain(150 * time.Millisecond)
	}
	if opts.TargetChannel > 0 {
		fmt.Fprintf(w, "\n# Driving to manual channel %d\n", opts.TargetChannel)
		if err := stepToChannel(opts.TargetChannel); err != nil {
			fmt.Fprintf(w, "# WARN: %v — continuing anyway\n", err)
		}
	}
	if opts.TargetRange != "" {
		fmt.Fprintf(w, "\n# Driving to range %q\n", opts.TargetRange)
		if err := stepToRange(opts.TargetRange); err != nil {
			fmt.Fprintf(w, "# WARN: %v — continuing anyway\n", err)
		}
	}

	modes := []string{originalMode}
	if opts.CycleModes {
		modes = []string{"power_swr", "waveform", "spectrum"}
	}

	for _, mode := range modes {
		if mode != "" && opts.CycleModes {
			fmt.Fprintf(w, "\n===== mode_step → %s =====\n", mode)
			if _, err := stepToMode(mode, 8); err != nil {
				fmt.Fprintf(w, "# %v\n", err)
				continue
			}
			drain(150 * time.Millisecond)
		}
		// Baseline: cmd '0' (live telemetry) — slot should be zero or
		// scope/spec residue if firmware streams regardless of mode.
		if _, err := captureOne('0', 4); err != nil {
			return err
		}
		// Then each sample cmd in turn.
		for cmd := byte('1'); cmd <= byte('5'); cmd++ {
			if _, err := captureOne(cmd, frames); err != nil {
				return err
			}
		}
		// And cmd '6' (status text) for comparison against
		// known-ASCII slot content.
		if _, err := captureOne('6', 4); err != nil {
			return err
		}
	}

	if opts.CycleModes && originalMode != "" {
		fmt.Fprintf(w, "\n# Restoring original top_mode=%q\n", originalMode)
		_, _ = stepToMode(originalMode, 8)
	}
	fmt.Fprintln(w, "\n# done.")
	return nil
}

// formatFullFrame prints all 64 bytes of an IN report. In sample modes
// (cmds '1'..'5' while top_mode=waveform/spectrum) the firmware
// repurposes the whole report as sample data, so the bytes-40..63
// "slot" abstraction doesn't apply; we have to see everything to spot
// the buffer layout.
//
// Output: hex (split as 0..7 / 8..15 / 16..39 / 40..63 for readability),
// then the same 64 bytes as u8 and u16BE summaries.
func formatFullFrame(w io.Writer, frame []byte, idx int) {
	hexFull := hex.EncodeToString(frame)
	// Per-byte u8.
	u8parts := make([]string, ReportSize)
	for i, b := range frame {
		u8parts[i] = fmt.Sprintf("%3d", b)
	}
	// 16-bit big-endian view (32 values).
	be := make([]string, ReportSize/2)
	for i := 0; i < ReportSize/2; i++ {
		be[i] = fmt.Sprintf("%5d", int(frame[2*i])<<8|int(frame[2*i+1]))
	}
	// Heuristic header: if byte[0] is in '0'..'?' (cmd-echo range) AND
	// every other byte is zero, this is a command echo.
	echo := frame[0] >= '0' && frame[0] <= '?'
	if echo {
		for i := 1; i < ReportSize; i++ {
			if frame[i] != 0 {
				echo = false
				break
			}
		}
	}
	tag := ""
	if echo {
		tag = " [ECHO]"
	}
	fmt.Fprintf(w, "  [%2d]%s b0=0x%02x b3=%d b4=%d b6=%d  hex=%s\n", idx, tag, frame[0], frame[3], frame[4], frame[6], hexFull)
	fmt.Fprintf(w, "       u8 :  %s\n", join(u8parts, " "))
	fmt.Fprintf(w, "       BE :  %s\n", join(be, " "))
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// HasLPMeterAttached returns true when the host has at least one HID
// whose product string matches LP-500 / LP-700, OR matches the default
// Microchip VID:PID. Used by the `auto` backend selection in main.
func HasLPMeterAttached() bool {
	devs, err := enumerateHID()
	if err != nil {
		return false
	}
	for _, d := range devs {
		if isLPMeter(d.Product) {
			return true
		}
		if d.VendorID == DefaultVendorID && d.ProductID == DefaultProductID {
			return true
		}
	}
	return false
}

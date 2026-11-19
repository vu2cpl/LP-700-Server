package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"lp700-server/internal/lpmeter"
)

// startHub runs a hub with one registered client, no heartbeats and a
// short trailing flush.
func startHub(t *testing.T) (chan<- lpmeter.Snapshot, *client, *Hub) {
	t.Helper()
	snapIn := make(chan lpmeter.Snapshot)
	h := NewHub(snapIn, nil, nil, &fakeSource{}, Options{Heartbeat: time.Hour, MaxClients: 4}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.flushEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)
	c := &client{send: make(chan []byte, 16), addr: "test"}
	h.register <- c
	return snapIn, c, h
}

func idleSnap(avgW float64) lpmeter.Snapshot {
	return lpmeter.Snapshot{
		Timestamp: time.Now(),
		Valid:     true,
		Channel:   1,
		PowerAvgW: avgW,
		SWR:       1,
		Range:     "auto",
		PeakMode:  "average",
		TopMode:   "power_swr",
	}
}

// nextTelemetry returns the next telemetry frame the client gets within d.
func nextTelemetry(t *testing.T, c *client, d time.Duration) (lpmeter.Snapshot, bool) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case data := <-c.send:
			var f struct {
				Type string           `json:"type"`
				Data lpmeter.Snapshot `json:"data"`
			}
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			if f.Type == "telemetry" {
				return f.Data, true
			}
		case <-timeout:
			return lpmeter.Snapshot{}, false
		}
	}
}

// TestTrailingFlushSendsHeldBackReading: after key-up the average fell
// 10 → 0.2 → 0 W. CloseEnough holds back the 0 W (inside its 0.5 W
// deadband of the 0.2 W sent last), so before the trailing flush clients
// and resyncs stayed on 0.2 W while the meter read 0 W (bench,
// 2026-11-19).
func TestTrailingFlushSendsHeldBackReading(t *testing.T) {
	snapIn, c, h := startHub(t)

	for _, w := range []float64{10, 0.2} {
		snapIn <- idleSnap(w)
		got, ok := nextTelemetry(t, c, time.Second)
		if !ok || got.PowerAvgW != w {
			t.Fatalf("after %.1f W: got %.1f W (ok=%v), want it sent at once", w, got.PowerAvgW, ok)
		}
	}

	snapIn <- idleSnap(0)
	got, ok := nextTelemetry(t, c, time.Second)
	if !ok || got.PowerAvgW != 0 {
		t.Fatalf("held-back 0 W: got %.1f W (ok=%v), want 0 W from the trailing flush", got.PowerAvgW, ok)
	}

	h.resync <- c
	got, ok = nextTelemetry(t, c, time.Second)
	if !ok || got.PowerAvgW != 0 {
		t.Errorf("resync after flush: got %.1f W (ok=%v), want 0 W", got.PowerAvgW, ok)
	}
}

// TestTrailingFlushIdleSendsNothing: repeated snapshots with the same
// readings (only the timestamp moves) must not produce extra frames.
func TestTrailingFlushIdleSendsNothing(t *testing.T) {
	snapIn, c, _ := startHub(t)

	snapIn <- idleSnap(0.2)
	if _, ok := nextTelemetry(t, c, time.Second); !ok {
		t.Fatal("first snapshot not sent")
	}
	for i := 0; i < 5; i++ {
		snapIn <- idleSnap(0.2)
	}
	if got, ok := nextTelemetry(t, c, 200*time.Millisecond); ok {
		t.Errorf("idle: got an extra frame (%.1f W), want none", got.PowerAvgW)
	}
}

package lpmeter

import (
	"testing"
	"time"
)

// TestSameReadings: the timestamp doesn't count; any reading does, also
// one inside CloseEnough's deadband.
func TestSameReadings(t *testing.T) {
	a := Snapshot{Timestamp: time.Now(), Valid: true, Channel: 1, PowerAvgW: 0.2, SWR: 1, Range: "auto", TopMode: "power_swr"}
	b := a
	b.Timestamp = a.Timestamp.Add(time.Second)
	if !a.SameReadings(b) {
		t.Error("snapshots that differ only in timestamp should have the same readings")
	}
	b.PowerAvgW = 0
	if a.SameReadings(b) {
		t.Error("0.2 W vs 0 W should differ")
	}
	if !a.CloseEnough(b) {
		t.Error("0.2 W vs 0 W is inside CloseEnough's deadband; the flush exists for this case")
	}
	c := a
	c.StatusMessage = "Reduce power or lower range"
	if a.SameReadings(c) {
		t.Error("a status message change should differ")
	}
}

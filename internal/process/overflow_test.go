package process

import (
	"context"
	"testing"
	"time"

	"github.com/wyw14/cry-97/internal/intake"
	"github.com/wyw14/cry-97/internal/model"
)

type mutableClock struct{ t time.Time }

func newOverflowPlant(t *testing.T, lines []model.LineID) (*Plant, *mutableClock) {
	t.Helper()
	clk := &mutableClock{t: time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)}
	plant, err := NewPlant(t.TempDir(), lines, func() time.Time { return clk.t })
	if err != nil {
		t.Fatalf("plant: %v", err)
	}
	return plant, clk
}

func levelSample(line model.LineID, basin string, seq uint64, level float64, at time.Time) model.Sample {
	return model.NewSample(line, basin, model.SampleWaterLevel, seq, level, "m", at)
}

func activeOverflowAlarm(plant *Plant, line model.LineID, code string) bool {
	for _, alarm := range plant.Alarms() {
		if alarm.LineID == line && alarm.Code == code && alarm.Active() {
			return true
		}
	}
	return false
}

func TestOverflowEndToEnd(t *testing.T) {
	ctx := context.Background()
	lines := []model.LineID{"line-1", "line-2"}
	plant, clk := newOverflowPlant(t, lines)

	batch1, err := plant.StartBatch(ctx, "line-1", 760, "north-intake")
	if err != nil {
		t.Fatalf("start batch 1: %v", err)
	}
	if _, err := plant.StartBatch(ctx, "line-2", 760, "south-intake"); err != nil {
		t.Fatalf("start batch 2: %v", err)
	}

	// High level on EQ-line-1 engages stop-level protection.
	if err := plant.ReceiveSample(ctx, levelSample("line-1", "EQ-line-1", 1, 5.8, clk.t)); err != nil {
		t.Fatalf("receive high level: %v", err)
	}
	view, ok := plant.OverflowView("EQ-line-1")
	if !ok || view.Level != intake.LevelStop {
		t.Fatalf("EQ-line-1 level=%v ok=%v want stop", view.Level, ok)
	}
	if view.BatchID != batch1.ID || view.LineID != "line-1" {
		t.Fatalf("protection not tied to active batch: line=%s batch=%s", view.LineID, view.BatchID)
	}
	if !activeOverflowAlarm(plant, "line-1", "OVERFLOW_INFLOW_STOP") {
		t.Fatal("expected critical stop alarm on line-1")
	}
	if activeOverflowAlarm(plant, "line-2", "OVERFLOW_INFLOW_STOP") {
		t.Fatal("line-2 must not carry line-1's overflow alarm")
	}

	var stoppedBasin string
	for _, command := range plant.devices.All() {
		if command.Action == "stop" {
			stoppedBasin = command.Metadata["basin_id"]
		}
	}
	if stoppedBasin != "EQ-line-1" {
		t.Fatalf("stop command targeted %q want EQ-line-1", stoppedBasin)
	}

	// A new admission on the protected line is refused...
	if _, err := plant.StartBatch(ctx, "line-1", 500, "north-intake"); err == nil {
		t.Fatal("intake on protected line must be refused")
	}
	// ...while the other equalizer keeps accepting intake.
	view2, _ := plant.OverflowView("EQ-line-2")
	if view2.Level != intake.LevelNormal {
		t.Fatalf("EQ-line-2 wrongly affected: %s", view2.Level)
	}

	// A late-arriving (stale timestamp) low reading is rejected before
	// journaling and must not release protection.
	late := levelSample("line-1", "EQ-line-1", 2, 2.0, clk.t.Add(-time.Minute))
	if err := plant.ReceiveSample(ctx, late); err == nil {
		t.Fatal("stale water level must be rejected")
	}
	view, _ = plant.OverflowView("EQ-line-1")
	if view.Level != intake.LevelStop || view.WaterLevelM != 5.8 {
		t.Fatalf("stale sample changed protection: level=%s water=%.2f", view.Level, view.WaterLevelM)
	}

	// A duplicate/older sequence is rejected without touching the latch.
	dup := levelSample("line-1", "EQ-line-1", 1, 2.0, clk.t.Add(time.Minute))
	if err := plant.ReceiveSample(ctx, dup); err == nil {
		t.Fatal("out-of-order water level must be rejected")
	}
	view, _ = plant.OverflowView("EQ-line-1")
	if view.Level != intake.LevelStop {
		t.Fatalf("duplicate sequence released protection: %s", view.Level)
	}
}

func TestOverflowRecoveryClearsAlarm(t *testing.T) {
	ctx := context.Background()
	plant, clk := newOverflowPlant(t, []model.LineID{"line-1"})
	if _, err := plant.StartBatch(ctx, "line-1", 760, "north-intake"); err != nil {
		t.Fatalf("start batch: %v", err)
	}
	if err := plant.ReceiveSample(ctx, levelSample("line-1", "EQ-line-1", 1, 5.8, clk.t)); err != nil {
		t.Fatalf("high: %v", err)
	}

	// Safe reading (3.0 m on 6.0 m full scale: tto ~63 min > 60 min horizon).
	clk.t = clk.t.Add(time.Minute)
	if err := plant.ReceiveSample(ctx, levelSample("line-1", "EQ-line-1", 2, 3.0, clk.t)); err != nil {
		t.Fatalf("safe 1: %v", err)
	}
	view, _ := plant.OverflowView("EQ-line-1")
	if view.Level != intake.LevelStop {
		t.Fatalf("latch must hold through the dwell period, got %s", view.Level)
	}

	// Second fresh safe reading after the 30s dwell releases the latch.
	clk.t = clk.t.Add(31 * time.Second)
	if err := plant.ReceiveSample(ctx, levelSample("line-1", "EQ-line-1", 3, 2.9, clk.t)); err != nil {
		t.Fatalf("safe 2: %v", err)
	}
	view, _ = plant.OverflowView("EQ-line-1")
	if view.Level != intake.LevelNormal {
		t.Fatalf("level=%s want normal after recovery", view.Level)
	}
	if activeOverflowAlarm(plant, "line-1", "OVERFLOW_INFLOW_STOP") {
		t.Fatal("stop alarm must be recovered automatically when protection clears")
	}
}

func TestEmergencyIsolationKeepsBasinLatch(t *testing.T) {
	ctx := context.Background()
	plant, clk := newOverflowPlant(t, []model.LineID{"line-1"})
	if _, err := plant.StartBatch(ctx, "line-1", 760, "north-intake"); err != nil {
		t.Fatalf("start batch: %v", err)
	}
	if err := plant.ReceiveSample(ctx, levelSample("line-1", "EQ-line-1", 1, 5.8, clk.t)); err != nil {
		t.Fatalf("high: %v", err)
	}
	if _, err := plant.EmergencyStop(ctx, "line-1", "operator test"); err != nil {
		t.Fatalf("emergency: %v", err)
	}
	view, _ := plant.OverflowView("EQ-line-1")
	if view.Level != intake.LevelStop {
		t.Fatalf("basin latch lost on emergency isolation: %s", view.Level)
	}
}

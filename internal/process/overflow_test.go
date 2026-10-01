package process

import (
	"context"
	"testing"
	"time"

	"github.com/wyw14/cry-97/internal/intake"
	"github.com/wyw14/cry-97/internal/model"
)

func newTestPlant(t *testing.T) (*Plant, func() time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	plant, err := NewPlant(t.TempDir(), []model.LineID{"line-1", "line-2", "line-3"}, clock)
	if err != nil {
		t.Fatalf("new plant: %v", err)
	}
	return plant, func() time.Time { return now }
}

func levelSample(basin string, seq uint64, volume float64, observed time.Time) model.Sample {
	return model.Sample{
		LineID: "line-1", BasinID: basin, Kind: model.SampleWaterLevel,
		Sequence: seq, Value: volume, Unit: "m3", ObservedAt: observed,
	}
}

func TestOverflowEndToEnd(t *testing.T) {
	plant, clock := newTestPlant(t)
	now := clock()
	ctx := context.Background()

	// line-1 admits onto EQ-line-1 (capacity 1600).
	batch, err := plant.StartBatch(ctx, "line-1", 760, "north-intake")
	if err != nil {
		t.Fatalf("start batch: %v", err)
	}
	basin := "EQ-line-1"

	// With 760 m3/h assigned: projected bands trigger at
	// 600 m3  -> projected 0.85 -> warning
	// 720 m3  -> projected 0.925 -> restricted
	// 800 m3  -> projected 0.975 -> stop
	if err := plant.ObserveWaterLevel(ctx, levelSample(basin, 1, 600, now)); err != nil {
		t.Fatalf("warning sample: %v", err)
	}
	status, err := plant.OverflowBasin(basin)
	if err != nil {
		t.Fatal(err)
	}
	if status.Level != intake.LevelWarning {
		t.Fatalf("want warning, got %s", status.Level)
	}
	if status.BatchID != batch.ID.String() {
		t.Fatalf("protection not linked to active batch: %+v", status)
	}
	if status.AlarmID == "" {
		t.Fatalf("warning should raise an alarm")
	}

	// Restricted by projection.
	if err := plant.ObserveWaterLevel(ctx, levelSample(basin, 2, 720, now.Add(time.Minute))); err != nil {
		t.Fatalf("restrict sample: %v", err)
	}
	status, _ = plant.OverflowBasin(basin)
	if status.Level != intake.LevelRestricted {
		t.Fatalf("want restricted, got %s", status.Level)
	}
	if admission, ok := plant.intake.Current("line-1"); !ok || !admission.Throttled {
		t.Fatalf("running admission should be throttled: %+v", admission)
	}

	// Stop by measured level (0.95+), regardless of the throttled projection.
	if err := plant.ObserveWaterLevel(ctx, levelSample(basin, 3, 1530, now.Add(2*time.Minute))); err != nil {
		t.Fatalf("stop sample: %v", err)
	}
	status, _ = plant.OverflowBasin(basin)
	if status.Level != intake.LevelStopped {
		t.Fatalf("want stopped, got %s", status.Level)
	}
	// The running admission must be closed.
	if admission, ok := plant.intake.Current("line-1"); ok && admission.FlowM3Hour != 0 {
		t.Fatalf("admission not closed: %+v", admission)
	}
	// A valve close command must target only this basin.
	var found bool
	for _, command := range plant.devices.All() {
		if command.DeviceID == "intake-valve-"+basin {
			if command.Action != "close" {
				t.Fatalf("want close command, got %s", command.Action)
			}
			found = true
		}
		if command.Kind == model.DeviceValve && command.DeviceID != "intake-valve-"+basin {
			t.Fatalf("protection touched another basin's valve: %s", command.DeviceID)
		}
	}
	if !found {
		t.Fatalf("no close command published")
	}
	// A critical stop alarm must exist for line-1.
	var critical bool
	for _, alarm := range plant.alarms.Active("line-1") {
		if alarm.Code == "INTAKE_OVERFLOW_STOP" && alarm.Severity == model.SeverityCritical {
			critical = true
		}
	}
	if !critical {
		t.Fatalf("critical stop alarm missing")
	}

	// Another equalizer must be untouched.
	other, _ := plant.OverflowBasin("EQ-line-2")
	if other.Level != intake.LevelNormal {
		t.Fatalf("other equalizer wrongly protected: %s", other.Level)
	}
}

func TestStaleReadingCannotClearStop(t *testing.T) {
	plant, clock := newTestPlant(t)
	now := clock()
	ctx := context.Background()
	if _, err := plant.StartBatch(ctx, "line-1", 760, "north-intake"); err != nil {
		t.Fatal(err)
	}
	basin := "EQ-line-1"
	if err := plant.ObserveWaterLevel(ctx, levelSample(basin, 5, 1530, now)); err != nil {
		t.Fatal(err)
	}
	// Older sequence with low level → ignored.
	if err := plant.ObserveWaterLevel(ctx, levelSample(basin, 4, 10, now.Add(time.Minute))); err == nil {
		t.Fatal("out-of-order reading should be rejected")
	}
	status, _ := plant.OverflowBasin(basin)
	if status.Level != intake.LevelStopped {
		t.Fatalf("stale reading cleared protection: %s", status.Level)
	}
	// Newer sequence but older timestamp → ignored.
	if err := plant.ObserveWaterLevel(ctx, levelSample(basin, 6, 10, now.Add(-time.Minute))); err == nil {
		t.Fatal("expired reading should be rejected")
	}
	status, _ = plant.OverflowBasin(basin)
	if status.Level != intake.LevelStopped {
		t.Fatalf("expired reading cleared protection: %s", status.Level)
	}
}

func TestRecoveryClearsStop(t *testing.T) {
	plant, clock := newTestPlant(t)
	now := clock()
	ctx := context.Background()
	if _, err := plant.StartBatch(ctx, "line-1", 760, "north-intake"); err != nil {
		t.Fatal(err)
	}
	basin := "EQ-line-1"
	_ = plant.ObserveWaterLevel(ctx, levelSample(basin, 1, 1530, now))
	// Recovery: fresh reading at 0.50 fill with zero assigned flow (released).
	if err := plant.ObserveWaterLevel(ctx, levelSample(basin, 2, 800, now.Add(time.Minute))); err != nil {
		t.Fatalf("recovery sample: %v", err)
	}
	status, _ := plant.OverflowBasin(basin)
	if status.Level != intake.LevelNormal {
		t.Fatalf("want normal after recovery, got %s", status.Level)
	}
	if plant.equalizers.IsBlocked(basin) {
		t.Fatalf("basin still blocked after recovery")
	}
	// No active overflow alarms should remain for line-1.
	for _, alarm := range plant.alarms.Active("line-1") {
		if alarm.Code == "INTAKE_OVERFLOW_STOP" {
			t.Fatalf("stop alarm not recovered")
		}
	}
}

func TestRestrictionThrottlesNewAdmission(t *testing.T) {
	plant, clock := newTestPlant(t)
	now := clock()
	ctx := context.Background()
	// Put the free EQ-line-3 into restricted protection (0.92 full, capacity
	// 1900 m3, no assigned flow so the projection does not jump to stop).
	if _, err := plant.ReportWaterLevel(ctx, "EQ-line-3", 1, 1748, "m3", now); err != nil {
		t.Fatal(err)
	}
	// Reserve the first two equalizers so line-3 can only land on EQ-line-3.
	if _, err := plant.StartBatch(ctx, "line-1", 500, "north"); err != nil {
		t.Fatal(err)
	}
	if _, err := plant.StartBatch(ctx, "line-2", 500, "south"); err != nil {
		t.Fatal(err)
	}
	// line-3 requests 760 m3/h; headroom to the stop trigger is
	// (0.95-0.92)*1900 = 57 m3/h, so it must be throttled, not rejected.
	batch, err := plant.StartBatch(ctx, "line-3", 760, "east")
	if err != nil {
		t.Fatalf("restricted admission should throttle: %v", err)
	}
	admission, ok := plant.intake.Current("line-3")
	if !ok {
		t.Fatal("admission missing")
	}
	if !admission.Throttled || admission.FlowM3Hour != 57 {
		t.Fatalf("admission = %+v, want throttled to 57", admission)
	}
	if admission.EqualizerID != "EQ-line-3" {
		t.Fatalf("wrong basin %s", admission.EqualizerID)
	}
	_ = batch
}

func TestStopDoesNotAffectAnotherEqualizerBatch(t *testing.T) {
	plant, clock := newTestPlant(t)
	now := clock()
	ctx := context.Background()
	// line-1 on EQ-line-1, line-2 on EQ-line-2.
	batch1, err := plant.StartBatch(ctx, "line-1", 500, "north")
	if err != nil {
		t.Fatal(err)
	}
	batch2, err := plant.StartBatch(ctx, "line-2", 500, "south")
	if err != nil {
		t.Fatal(err)
	}
	// EQ-line-1 overflows to stop; line-2 on EQ-line-2 must keep running.
	if err := plant.ObserveWaterLevel(ctx, levelSample("EQ-line-1", 1, 1530, now)); err != nil {
		t.Fatal(err)
	}
	admission2, ok := plant.intake.Current("line-2")
	if !ok || admission2.FlowM3Hour != 500 {
		t.Fatalf("line-2 admission affected: %+v", admission2)
	}
	if !batch2.IsActive() || !batch1.IsActive() {
		t.Fatal("both batches should still exist")
	}
	// The stop alarm belongs to line-1 only.
	for _, alarm := range plant.alarms.Active("line-2") {
		if alarm.Code == "INTAKE_OVERFLOW_STOP" {
			t.Fatalf("stop alarm leaked to line-2")
		}
	}
	if len(plant.alarms.Active("line-1")) == 0 {
		t.Fatal("line-1 should have the stop alarm")
	}
}

func TestReportWaterLevelByBasin(t *testing.T) {
	plant, clock := newTestPlant(t)
	now := clock()
	ctx := context.Background()
	// Assign line-1 so the basin has a line link.
	if _, err := plant.StartBatch(ctx, "line-1", 760, "north-intake"); err != nil {
		t.Fatal(err)
	}
	status, err := plant.ReportWaterLevel(ctx, "EQ-line-1", 1, 1530, "m3", now)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if status.Level != intake.LevelStopped {
		t.Fatalf("want stopped via basin report, got %s", status.Level)
	}
}

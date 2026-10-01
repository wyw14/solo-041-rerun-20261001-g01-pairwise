package intake

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type clock struct{ t time.Time }

func (c *clock) nowFunc() time.Time { return c.t }
func (c *clock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

type fakeResolver struct {
	id uuid.UUID
	ok bool
}

func (f fakeResolver) ActiveBatchID(model.LineID) (uuid.UUID, bool) { return f.id, f.ok }

type recorder struct{ notices []ProtectionNotice }

func (r *recorder) ProtectionChanged(n ProtectionNotice) { r.notices = append(r.notices, n) }

func testGuard(t *testing.T, basins []Equalizer, th Thresholds) (*OverflowGuard, *EqualizationPool, *clock, *recorder) {
	t.Helper()
	pool, err := NewEqualizationPool(basins)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	clk := &clock{t: time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)}
	guard, err := NewOverflowGuard(pool, th, clk.nowFunc)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	rec := &recorder{}
	guard.Bind(nil, rec)
	return guard, pool, clk, rec
}

func sampleFor(basin string, seq uint64, level float64, at time.Time) model.Sample {
	return model.NewSample("line-1", basin, model.SampleWaterLevel, seq, level, "m", at)
}

func TestLevelThresholdEngagements(t *testing.T) {
	th := DefaultThresholds()
	th.MinRecoveryDwell = time.Millisecond
	guard, pool, clk, rec := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
	}, th)
	if _, err := pool.Assign("line-1", 100, clk.t); err != nil {
		t.Fatalf("assign: %v", err)
	}

	cases := []struct {
		name  string
		level float64
		want  ProtectionLevel
	}{
		{"normal below warning", 7.0, LevelNormal},
		{"warning at 80%", 8.0, LevelWarning},
		{"restrict at 90%", 9.0, LevelRestrict},
		{"stop at 95%", 9.5, LevelStop},
	}
	seq := uint64(1)
	for _, tc := range cases {
		_, accepted, err := guard.Ingest(sampleFor("EQ-A", seq, tc.level, clk.t))
		if err != nil || !accepted {
			t.Fatalf("%s: ingest accepted=%v err=%v", tc.name, accepted, err)
		}
		view, _ := guard.View("EQ-A")
		if view.Level != tc.want {
			t.Fatalf("%s: level=%s want %s", tc.name, view.Level, tc.want)
		}
		seq++
		clk.advance(time.Minute)
	}
	if len(rec.notices) != 3 {
		t.Fatalf("expected 3 transition notices, got %d", len(rec.notices))
	}
	if rec.notices[2].After != LevelStop {
		t.Fatalf("last notice after=%s", rec.notices[2].After)
	}
}

func TestTimeToOverflowTriggersBeforeLevelRule(t *testing.T) {
	th := DefaultThresholds()
	th.MinRecoveryDwell = time.Millisecond
	guard, pool, clk, _ := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1800, FullLevelM: 10},
	}, th)
	// Level 79.5% (below the 80% level warning) but freeboard 369 m3 at
	// 1500 m3/h overflows in ~14.8 minutes -> stop horizon.
	if _, err := pool.Assign("line-1", 1500, clk.t); err != nil {
		t.Fatalf("assign: %v", err)
	}
	_, _, err := guard.Ingest(sampleFor("EQ-A", 1, 7.95, clk.t))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	view, _ := guard.View("EQ-A")
	if view.Level != LevelStop {
		t.Fatalf("level=%s want stop", view.Level)
	}
	if view.Basis == nil || view.Basis.Rule != "time_to_overflow" {
		t.Fatalf("basis=%+v want time_to_overflow rule", view.Basis)
	}
	if view.TimeToOverflowMinutes == nil || *view.TimeToOverflowMinutes > 15 {
		t.Fatalf("time to overflow=%v want <=15 min", view.TimeToOverflowMinutes)
	}
}

func TestExpiredAndOutOfOrderSamplesCannotRelease(t *testing.T) {
	th := DefaultThresholds()
	th.MinRecoveryDwell = time.Millisecond
	guard, _, clk, _ := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
	}, th)
	t0 := clk.t
	// Engage stop at seq 1.
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 1, 9.8, t0)); err != nil {
		t.Fatalf("ingest high: %v", err)
	}
	view, _ := guard.View("EQ-A")
	if view.Level != LevelStop {
		t.Fatalf("level=%s want stop", view.Level)
	}

	stale := []struct {
		name string
		s    model.Sample
	}{
		{"older sequence", sampleFor("EQ-A", 0, 2.0, t0.Add(time.Minute))},
		{"equal sequence older timestamp", sampleFor("EQ-A", 1, 2.0, t0.Add(-time.Minute))},
		{"newer sequence but older timestamp", sampleFor("EQ-A", 2, 2.0, t0.Add(-time.Second))},
	}
	for _, tc := range stale {
		_, accepted, err := guard.Ingest(tc.s)
		if err != nil {
			t.Fatalf("%s: unexpected err %v", tc.name, err)
		}
		if accepted {
			t.Fatalf("%s: stale sample must be rejected", tc.name)
		}
		view, _ := guard.View("EQ-A")
		if view.Level != LevelStop {
			t.Fatalf("%s: stale reading released protection, level=%s", tc.name, view.Level)
		}
		if view.WaterLevelM != 9.8 {
			t.Fatalf("%s: stale reading changed level to %.2f", tc.name, view.WaterLevelM)
		}
	}
}

func TestLatchRequiresRecoveryDwell(t *testing.T) {
	th := DefaultThresholds()
	th.MinRecoveryDwell = 50 * time.Millisecond
	guard, _, clk, rec := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
	}, th)
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 1, 9.8, clk.t)); err != nil {
		t.Fatalf("ingest high: %v", err)
	}
	// First safe reading starts the dwell timer but must not release yet.
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 2, 6.5, clk.t.Add(time.Minute))); err != nil {
		t.Fatalf("ingest safe 1: %v", err)
	}
	view, _ := guard.View("EQ-A")
	if view.Level != LevelStop {
		t.Fatalf("protection released before dwell, level=%s", view.Level)
	}
	if view.Recovery == nil || view.Recovery.Satisfied {
		t.Fatalf("recovery must report not satisfied during dwell: %+v", view.Recovery)
	}
	// Second safe reading after the dwell window releases the latch.
	clk.advance(2 * time.Minute)
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 3, 6.4, clk.t)); err != nil {
		t.Fatalf("ingest safe 2: %v", err)
	}
	view, _ = guard.View("EQ-A")
	if view.Level != LevelNormal {
		t.Fatalf("level=%s want normal after dwell", view.Level)
	}
	last := rec.notices[len(rec.notices)-1]
	if last.Before != LevelStop || last.After != LevelNormal {
		t.Fatalf("release notice=%+v", last)
	}
}

func TestStaleMeasurementCannotRecover(t *testing.T) {
	th := DefaultThresholds()
	th.MinRecoveryDwell = time.Millisecond
	th.MaxSampleAge = 10 * time.Minute
	guard, _, clk, _ := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
	}, th)
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 1, 9.8, clk.t)); err != nil {
		t.Fatalf("ingest high: %v", err)
	}
	// Safe level but the observation is older than the freshness window.
	clk.advance(30 * time.Minute)
	old := sampleFor("EQ-A", 2, 6.0, clk.t.Add(-20*time.Minute))
	if _, _, err := guard.Ingest(old); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	view, _ := guard.View("EQ-A")
	if view.Level != LevelStop || !view.SampleStale {
		t.Fatalf("level=%s stale=%v, want stop+stale", view.Level, view.SampleStale)
	}
}

func TestBasinIsolationNeverStopsAnotherEqualizer(t *testing.T) {
	th := DefaultThresholds()
	th.MinRecoveryDwell = time.Millisecond
	guard, pool, clk, _ := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
		{ID: "EQ-B", Capacity: 1000, FullLevelM: 10},
	}, th)
	a, err := pool.Assign("line-1", 100, clk.t)
	if err != nil {
		t.Fatalf("assign a: %v", err)
	}
	b, err := pool.Assign("line-2", 100, clk.t)
	if err != nil {
		t.Fatalf("assign b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("lines must map to distinct basins, both %s", a.ID)
	}
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 1, 9.9, clk.t)); err != nil {
		t.Fatalf("ingest a: %v", err)
	}
	viewA, _ := guard.View("EQ-A")
	if viewA.Level != LevelStop {
		t.Fatalf("EQ-A level=%s want stop", viewA.Level)
	}
	// line-1 must be blocked...
	if decision, err := guard.Admission("line-1", 100); err != nil || decision.Allowed {
		t.Fatalf("line-1 admission should be blocked: %+v err=%v", decision, err)
	}
	// ...while line-2 on EQ-B stays fully open.
	decision, err := guard.Admission("line-2", 100)
	if err != nil || !decision.Allowed || decision.AdjustedFlow != 100 {
		t.Fatalf("line-2 admission wrongly affected: %+v err=%v", decision, err)
	}
	if decision.BasinID != "EQ-B" {
		t.Fatalf("line-2 resolved to %s want EQ-B", decision.BasinID)
	}
}

func TestRestrictionCutsFlow(t *testing.T) {
	th := DefaultThresholds()
	th.MinRecoveryDwell = time.Millisecond
	guard, pool, clk, _ := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 2000, FullLevelM: 10},
	}, th)
	// 91% level engages restrict; the modest 600 m3/h inflow keeps the
	// time-to-overflow at ~18 min so the time rule does not escalate to stop.
	if _, err := pool.Assign("line-1", 600, clk.t); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 1, 9.1, clk.t)); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	decision, err := guard.Admission("line-1", 1000)
	if err != nil || !decision.Allowed {
		t.Fatalf("restricted admission: %+v err=%v", decision, err)
	}
	if want := 1000 * th.RestrictFlowFactor; decision.AdjustedFlow != want {
		t.Fatalf("adjusted flow=%.1f want %.1f", decision.AdjustedFlow, want)
	}
}

func TestBatchAssociationOnEngagement(t *testing.T) {
	th := DefaultThresholds()
	batchID := uuid.New()
	guard, pool, clk, rec := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
	}, th)
	guard.Bind(fakeResolver{id: batchID, ok: true}, rec)
	if _, err := pool.Assign("line-1", 100, clk.t); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 1, 9.8, clk.t)); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	notice := rec.notices[len(rec.notices)-1]
	if notice.LineID != "line-1" || notice.BatchID != batchID {
		t.Fatalf("notice association line=%s batch=%s", notice.LineID, notice.BatchID)
	}
	view, _ := guard.View("EQ-A")
	if view.BatchID != batchID {
		t.Fatalf("view batch=%s want %s", view.BatchID, batchID)
	}
}

func TestDetachLineKeepsLatch(t *testing.T) {
	th := DefaultThresholds()
	guard, pool, clk, _ := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
	}, th)
	if _, err := pool.Assign("line-1", 100, clk.t); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 1, 9.8, clk.t)); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Emergency isolation detaches the association and releases the pool;
	// the basin-level latch and measurement history must survive.
	guard.DetachLine("line-1")
	pool.Release("line-1")
	view, _ := guard.View("EQ-A")
	if view.Level != LevelStop {
		t.Fatalf("detach released protection: level=%s", view.Level)
	}
	if view.LineID != "" || view.BatchID != uuid.Nil {
		t.Fatalf("association should be cleared: line=%s batch=%s", view.LineID, view.BatchID)
	}
}

func TestResetReEngagesWhenStillUnsafe(t *testing.T) {
	th := DefaultThresholds()
	guard, _, clk, rec := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
	}, th)
	if _, _, err := guard.Ingest(sampleFor("EQ-A", 1, 9.8, clk.t)); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	before := len(rec.notices)
	view, err := guard.Reset("EQ-A")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if view.Level != LevelStop {
		t.Fatalf("reset must re-engage while unsafe, level=%s", view.Level)
	}
	if len(rec.notices) <= before {
		t.Fatalf("expected re-engagement notice after reset")
	}
}

func TestUnknownBasinAndBadSample(t *testing.T) {
	guard, _, clk, _ := testGuard(t, []Equalizer{
		{ID: "EQ-A", Capacity: 1000, FullLevelM: 10},
	}, DefaultThresholds())
	if _, _, err := guard.Ingest(sampleFor("EQ-Z", 1, 5, clk.t)); err == nil {
		t.Fatal("expected error for unknown basin")
	}
	wrong := model.NewSample("line-1", "EQ-A", model.SampleCOD, 1, 5, "mg", clk.t)
	if _, _, err := guard.Ingest(wrong); err == nil {
		t.Fatal("expected error for non-level sample")
	}
}

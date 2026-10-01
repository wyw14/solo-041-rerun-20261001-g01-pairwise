package intake

import (
	"testing"
	"time"
)

func testProtection(t *testing.T) *Protection {
	t.Helper()
	book, err := NewProtection(
		[]string{"EQ-1", "EQ-2"},
		map[string]float64{"EQ-1": 1000, "EQ-2": 2000},
		DefaultThresholds,
	)
	if err != nil {
		t.Fatalf("new protection: %v", err)
	}
	return book
}

func at(minute int) time.Time {
	return time.Date(2026, 10, 1, 8, minute, 0, 0, time.UTC)
}

func reading(seq uint64, volume float64, minute int) Reading {
	return Reading{BasinID: "EQ-1", Sequence: seq, VolumeM3: volume, ObservedAt: at(minute)}
}

func TestOverflowBandsByLevel(t *testing.T) {
	book := testProtection(t)
	cases := []struct {
		seq    uint64
		volume float64
		want   ProtectionLevel
	}{
		{1, 790, LevelNormal},
		{2, 800, LevelWarning},
		{3, 899, LevelWarning},
		{4, 900, LevelRestricted},
		{5, 950, LevelStopped},
	}
	for _, tc := range cases {
		out := book.Observe(reading(tc.seq, tc.volume, int(tc.seq)), "", 0, at(int(tc.seq)))
		if out.After != tc.want {
			t.Fatalf("volume %.0f: got %s want %s", tc.volume, out.After, tc.want)
		}
	}
}

func TestOverflowProjectionTriggersEarly(t *testing.T) {
	book := testProtection(t)
	// 850/1000 = 0.85 (below restrict) but + 100 m3/h * 1h => 0.95 stop.
	out := book.Observe(reading(1, 850, 1), "line-1", 100, at(1))
	if out.After != LevelStopped {
		t.Fatalf("projection stop: got %s want stopped", out.After)
	}
	if out.Basis == nil || out.Basis.TriggeredBy != "projection" {
		t.Fatalf("basis should cite projection: %+v", out.Basis)
	}
}

func TestOutOfOrderReadingCannotClear(t *testing.T) {
	book := testProtection(t)
	book.Observe(reading(10, 960, 10), "", 0, at(10))
	if level, _ := book.LevelOf("EQ-1"); level != LevelStopped {
		t.Fatalf("setup: want stopped, got %s", level)
	}
	// A late, out-of-sequence "low" reading must be ignored.
	out := book.Observe(reading(9, 100, 11), "", 0, at(11))
	if !out.Ignored || out.IgnoreCause != "out_of_order" {
		t.Fatalf("want ignored out_of_order, got %+v", out)
	}
	if level, _ := book.LevelOf("EQ-1"); level != LevelStopped {
		t.Fatalf("stale reading cleared protection: %s", level)
	}
}

func TestStaleTimestampCannotClear(t *testing.T) {
	book := testProtection(t)
	book.Observe(reading(5, 960, 10), "", 0, at(10))
	// Newer sequence but older observation timestamp.
	out := book.Observe(reading(6, 100, 5), "", 0, at(12))
	if !out.Ignored || out.IgnoreCause != "stale_observation" {
		t.Fatalf("want ignored stale_observation, got %+v", out)
	}
	if level, _ := book.LevelOf("EQ-1"); level != LevelStopped {
		t.Fatalf("expired reading cleared protection: %s", level)
	}
}

func TestRetransmissionCannotLower(t *testing.T) {
	book := testProtection(t)
	book.Observe(reading(3, 960, 3), "", 0, at(3))
	out := book.Observe(reading(3, 100, 4), "", 0, at(4))
	if !out.Accepted || out.After != LevelStopped {
		t.Fatalf("retransmission lowered protection: %+v", out)
	}
}

func TestHysteresisRecoveryRequiresFreshSample(t *testing.T) {
	book := testProtection(t)
	book.Observe(reading(1, 960, 1), "", 0, at(1))
	// 0.90 is below release-stopped 0.88? no — 0.90 >= 0.88, stays stopped.
	out := book.Observe(reading(2, 900, 2), "", 0, at(2))
	if out.After != LevelStopped {
		t.Fatalf("0.90 should stay stopped, got %s", out.After)
	}
	// Below 0.88 but above 0.85 → drops to restricted.
	out = book.Observe(reading(3, 870, 3), "", 0, at(3))
	if out.After != LevelRestricted {
		t.Fatalf("0.87 should release to restricted, got %s", out.After)
	}
	// Below 0.85 but above 0.80 → warning.
	out = book.Observe(reading(4, 820, 4), "", 0, at(4))
	if out.After != LevelWarning {
		t.Fatalf("0.82 should release to warning, got %s", out.After)
	}
	// Below warn → normal.
	out = book.Observe(reading(5, 700, 5), "", 0, at(5))
	if out.After != LevelNormal {
		t.Fatalf("0.70 should clear, got %s", out.After)
	}
}

func TestProjectionBlocksRecovery(t *testing.T) {
	book := testProtection(t)
	book.Observe(reading(1, 960, 1), "", 0, at(1))
	// Level 0.86 clears release-stopped, but 0.86 + 50m3/h = 0.91 projected:
	// still at/over 0.88, so must remain stopped.
	out := book.Observe(reading(2, 860, 2), "line-1", 50, at(2))
	if out.After != LevelStopped {
		t.Fatalf("projection should hold stop, got %s", out.After)
	}
}

func TestProtectionIsPerBasin(t *testing.T) {
	book := testProtection(t)
	out := book.Observe(reading(1, 960, 1), "", 0, at(1))
	_ = out
	level1, _ := book.LevelOf("EQ-1")
	level2, _ := book.LevelOf("EQ-2")
	if level1 != LevelStopped || level2 != LevelNormal {
		t.Fatalf("EQ-1=%s EQ-2=%s, want stopped/normal", level1, level2)
	}
	// EQ-2 readings stay independent.
	out2 := book.Observe(Reading{BasinID: "EQ-2", Sequence: 1, VolumeM3: 100, ObservedAt: at(1)}, "", 0, at(1))
	if out2.After != LevelNormal {
		t.Fatalf("EQ-2 changed unexpectedly: %s", out2.After)
	}
}

func TestRestrictedFlowCap(t *testing.T) {
	book := testProtection(t)
	// 0.92 fill with zero assigned flow → restricted.
	book.Observe(reading(1, 920, 1), "line-1", 0, at(1))
	// Request 100 m3/h: headroom to 0.95 is 30 m3/h -> cap 30.
	decision, known := book.PermitFlow("EQ-1", 100)
	if !known || decision.Level != LevelRestricted {
		t.Fatalf("want restricted decision, got %+v", decision)
	}
	if decision.AllowedFlow != 30 {
		t.Fatalf("cap = %.1f, want 30", decision.AllowedFlow)
	}
	// A small request is still cut to the restricted-flow fraction (50% -> 10).
	decision, _ = book.PermitFlow("EQ-1", 20)
	if decision.AllowedFlow != 10 {
		t.Fatalf("small request cap = %.1f, want 10", decision.AllowedFlow)
	}
}

func TestRestrictedFractionCap(t *testing.T) {
	book := testProtection(t)
	// 0.91 fill: budget to stop is 40 m3/h; fraction of a 60 m3/h request is
	// 30 m3/h and wins.
	book.Observe(reading(1, 910, 1), "line-1", 0, at(1))
	decision, _ := book.PermitFlow("EQ-1", 60)
	if decision.Level != LevelRestricted || decision.AllowedFlow != 30 {
		t.Fatalf("want fraction cap 30, got %+v", decision)
	}
}

func TestStoppedPermitsNoFlow(t *testing.T) {
	book := testProtection(t)
	book.Observe(reading(1, 960, 1), "", 0, at(1))
	decision, known := book.PermitFlow("EQ-1", 10)
	if !known || decision.Level != LevelStopped || decision.AllowedFlow != 0 {
		t.Fatalf("want zero-flow stop, got %+v", decision)
	}
}

func TestUnknownBasinRejected(t *testing.T) {
	book := testProtection(t)
	out := book.Observe(Reading{BasinID: "EQ-X", Sequence: 1, VolumeM3: 1, ObservedAt: at(1)}, "", 0, at(1))
	if !out.Ignored || out.IgnoreCause != "unknown_basin" {
		t.Fatalf("want unknown_basin, got %+v", out)
	}
}

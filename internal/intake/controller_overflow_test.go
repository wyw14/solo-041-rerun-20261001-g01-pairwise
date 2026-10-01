package intake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

func newGuardedController(t *testing.T) (*Controller, *Protection) {
	t.Helper()
	pool, err := NewEqualizationPool([]Equalizer{
		{ID: "EQ-1", Capacity: 1000},
		{ID: "EQ-2", Capacity: 2000},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []model.LineID{"line-1", "line-2"} {
		if err := controller.Configure(line, 1800); err != nil {
			t.Fatal(err)
		}
	}
	protection, err := NewProtection(
		[]string{"EQ-1", "EQ-2"},
		map[string]float64{"EQ-1": 1000, "EQ-2": 2000},
		DefaultThresholds,
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.BindProtection(protection)
	return controller, protection
}

func TestAdmissionRejectedWhenStopped(t *testing.T) {
	controller, protection := newGuardedController(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	protection.Observe(Reading{BasinID: "EQ-1", Sequence: 1, VolumeM3: 960, ObservedAt: now}, "", 0, now)
	protection.Observe(Reading{BasinID: "EQ-2", Sequence: 1, VolumeM3: 1920, ObservedAt: now}, "", 0, now)

	_, err := controller.Admit(context.Background(), "line-1", 500, "north", now)
	var limited *FlowLimitedError
	if !errors.As(err, &limited) || limited.Level != LevelStopped {
		t.Fatalf("want stopped FlowLimitedError, got %v", err)
	}
	// The rejected attempt must not have reserved any basin.
	for _, basin := range controller.equalizers.Snapshot() {
		if basin.LineID != "" {
			t.Fatalf("stopped basin %s was reserved by %s", basin.ID, basin.LineID)
		}
	}
}

func TestAdmissionRoutesAroundStoppedBasin(t *testing.T) {
	controller, protection := newGuardedController(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	protection.Observe(Reading{BasinID: "EQ-1", Sequence: 1, VolumeM3: 960, ObservedAt: now}, "", 0, now)

	admission, err := controller.Admit(context.Background(), "line-1", 500, "north", now)
	if err != nil {
		t.Fatalf("line should route to the healthy equalizer: %v", err)
	}
	if admission.EqualizerID != "EQ-2" {
		t.Fatalf("want EQ-2, got %s", admission.EqualizerID)
	}
}

func TestAdmissionThrottledWhenRestricted(t *testing.T) {
	controller, protection := newGuardedController(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	protection.Observe(Reading{BasinID: "EQ-2", Sequence: 1, VolumeM3: 1840, ObservedAt: now}, "", 0, now)
	// EQ-2 is the first basin with capacity for a large flow... sort order puts
	// EQ-1 first; force EQ-2 by filling EQ-1 with line-2? Instead request small
	// flow: EQ-1 (restricted? no, only EQ-2 is). EQ-1 will be picked. To target
	// EQ-2 directly, reserve EQ-1 first.
	if _, err := controller.Admit(context.Background(), "line-2", 100, "south", now); err != nil {
		t.Fatal(err)
	}
	// line-1 can only land on EQ-2 (0.92 full). Request 200: headroom to 0.95
	// over 1h is 60 m3/h -> throttled to 60.
	admission, err := controller.Admit(context.Background(), "line-1", 200, "north", now)
	if err != nil {
		t.Fatalf("restricted admission should throttle, not reject: %v", err)
	}
	if !admission.Throttled || admission.FlowM3Hour != 60 {
		t.Fatalf("admission = %+v, want throttled to 60", admission)
	}
	if admission.EqualizerID != "EQ-2" {
		t.Fatalf("wrong basin: %s", admission.EqualizerID)
	}
}

func TestStopOnOneBasinDoesNotBlockOther(t *testing.T) {
	controller, protection := newGuardedController(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	protection.Observe(Reading{BasinID: "EQ-1", Sequence: 1, VolumeM3: 960, ObservedAt: now}, "", 0, now)

	// line-2 can still take EQ-2.
	admission, err := controller.Admit(context.Background(), "line-2", 500, "south", now)
	if err != nil {
		t.Fatalf("other equalizer must stay admittable: %v", err)
	}
	if admission.EqualizerID != "EQ-2" {
		t.Fatalf("want EQ-2, got %s", admission.EqualizerID)
	}
}

func TestStoppedBasinCannotBeReassigned(t *testing.T) {
	controller, protection := newGuardedController(t)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	first, err := controller.Admit(context.Background(), "line-1", 500, "north", now)
	if err != nil {
		t.Fatal(err)
	}
	// Basin goes into stop; the pool blocks it after the plant releases.
	protection.Observe(Reading{BasinID: first.EqualizerID, Sequence: 1, VolumeM3: 960, ObservedAt: now}, "line-1", 500, now)
	controller.equalizers.Release("line-1")
	controller.equalizers.BlockBasin(first.EqualizerID)

	// New lines must skip the blocked basin.
	admission, err := controller.Admit(context.Background(), "line-2", 500, "south", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("expected assignment on another basin: %v", err)
	}
	if admission.EqualizerID == first.EqualizerID {
		t.Fatalf("assigned back onto blocked basin %s", admission.EqualizerID)
	}
}

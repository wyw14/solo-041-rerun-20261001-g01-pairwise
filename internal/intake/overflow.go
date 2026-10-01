package intake

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ProtectionLevel is the overflow protection state of one basin.
type ProtectionLevel string

const (
	LevelNormal     ProtectionLevel = "normal"
	LevelWarning    ProtectionLevel = "warning"
	LevelRestricted ProtectionLevel = "restricted"
	LevelStopped    ProtectionLevel = "stopped"
)

// Default policy, in fraction of basin capacity. The release thresholds are
// below the trigger thresholds so a normal fluctuation at the band edge cannot
// churn protection on and off.
var DefaultThresholds = Thresholds{
	WarnAt: 0.80, RestrictAt: 0.90, StopAt: 0.95,
	ReleaseRestrictedAt: 0.85, ReleaseStoppedAt: 0.88,
	ProjectionHours: 1.0,
	// Restricted inflow is cut to this fraction of the current/requested flow,
	// but never above the headroom budget to the stop trigger.
	RestrictedFlowFraction: 0.5,
}

// Thresholds controls band triggers and the assigned-flow projection.
type Thresholds struct {
	WarnAt              float64
	RestrictAt          float64
	StopAt              float64
	ReleaseRestrictedAt float64
	ReleaseStoppedAt    float64
	// ProjectionHours is how far ahead the current assigned inflow is projected.
	// A basin nominally below a threshold but projected to reach it within this
	// window enters the band early.
	ProjectionHours float64
	// RestrictedFlowFraction caps restricted inflow to this fraction of the
	// requested/current flow.
	RestrictedFlowFraction float64
}

func (t Thresholds) valid() error {
	ordered := t.WarnAt > 0 && t.WarnAt < t.RestrictAt &&
		t.RestrictAt < t.StopAt && t.StopAt <= 1 &&
		t.ReleaseRestrictedAt > 0 && t.ReleaseRestrictedAt < t.ReleaseStoppedAt &&
		t.ReleaseStoppedAt < t.StopAt && t.ProjectionHours > 0 &&
		t.RestrictedFlowFraction > 0 && t.RestrictedFlowFraction <= 1
	if !ordered {
		return errors.New("overflow thresholds are inconsistent")
	}
	return nil
}

// TriggerBasis records exactly why a basin is in its current level so an
// operator can audit the protection action.
type TriggerBasis struct {
	Code           string    `json:"code"`
	Level          float64   `json:"level"`           // m³ currently stored
	LevelRatio     float64   `json:"level_ratio"`     // level / capacity
	Capacity       float64   `json:"capacity"`        // m³
	AssignedFlow   float64   `json:"assigned_flow"`   // m³/h from the active line assignment
	Projected      float64   `json:"projected_level"` // level after ProjectionHours at AssignedFlow
	ProjectedRatio float64   `json:"projected_ratio"`
	TriggeredBy    string    `json:"triggered_by"` // "level" or "projection"
	Threshold      float64   `json:"threshold"`    // fraction of capacity that was crossed
	Sequence       uint64    `json:"sequence"`
	ObservedAt     time.Time `json:"observed_at"`
}

// ProtectionStatus is the operator-facing state of one basin.
type ProtectionStatus struct {
	BasinID string `json:"basin_id"`
	LineID  string `json:"line_id,omitempty"`
	BatchID string `json:"batch_id,omitempty"`
	AlarmID string `json:"alarm_id,omitempty"`
	// AllowedFlow is the maximum inflow (m³/h) the protection currently admits
	// for the active assignment. It is zero while stopped.
	AllowedFlow float64         `json:"allowed_flow"`
	Level       ProtectionLevel `json:"level"`
	FillRatio   float64         `json:"fill_ratio"`
	Headroom    float64         `json:"headroom_m3"`
	Basis       *TriggerBasis   `json:"basis,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
	// Recovery describes what must hold before the level can be lowered.
	Recovery RecoveryCondition `json:"recovery"`
}

// RecoveryCondition is the latch-release rule for the current level.
type RecoveryCondition struct {
	RequiredRatio float64   `json:"required_fill_ratio"`
	Description   string    `json:"description"`
	RequiresFresh bool      `json:"requires_fresh_sample"`
	Since         time.Time `json:"since,omitempty"`
}

type basinProtection struct {
	level       ProtectionLevel
	basis       *TriggerBasis
	latestSeq   uint64
	latestAt    time.Time
	fillRatio   float64
	allowedFlow float64
	activeSince time.Time
}

// Protection is the overflow-protection book for the intake and equalization
// basins. It is keyed strictly by basin id: state for one basin never affects
// another basin.
type Protection struct {
	mu         sync.Mutex
	basins     map[string]*basinProtection
	capacity   map[string]float64
	thresholds Thresholds
}

func NewProtection(basinIDs []string, capacities map[string]float64, thresholds Thresholds) (*Protection, error) {
	if thresholds == (Thresholds{}) {
		thresholds = DefaultThresholds
	}
	if err := thresholds.valid(); err != nil {
		return nil, err
	}
	if len(basinIDs) == 0 {
		return nil, errors.New("overflow protection needs at least one basin")
	}
	book := &Protection{
		basins:     make(map[string]*basinProtection, len(basinIDs)),
		capacity:   make(map[string]float64, len(basinIDs)),
		thresholds: thresholds,
	}
	for _, id := range basinIDs {
		capacity := capacities[id]
		if id == "" || capacity <= 0 {
			return nil, errors.New("overflow protection basin identity and capacity are required")
		}
		book.capacity[id] = capacity
		book.basins[id] = &basinProtection{level: LevelNormal}
	}
	return book, nil
}

// Reading is a water-level sample reported for one basin. VolumeM3 is the
// measured volume currently in the basin.
type Reading struct {
	BasinID    string
	Sequence   uint64
	VolumeM3   float64
	ObservedAt time.Time
}

// Outcome reports what happened after ingesting a reading.
type Outcome struct {
	BasinID     string
	Before      ProtectionLevel
	After       ProtectionLevel
	AllowedFlow float64
	Accepted    bool
	Ignored     bool
	IgnoreCause string
	Basis       *TriggerBasis
}

// Observe ingests one water-level reading together with the current line
// assignment for the basin. Out-of-order (lower sequence) or expired (older
// timestamp) readings are ignored and can NEVER clear protection: the latch
// stays untouched. A retransmission of the current sequence also cannot lower
// the level.
func (p *Protection) Observe(reading Reading, lineID string, assignedFlow float64, now time.Time) Outcome {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, ok := p.basins[reading.BasinID]
	out := Outcome{BasinID: reading.BasinID}
	if !ok {
		out.Ignored, out.IgnoreCause = true, "unknown_basin"
		return out
	}
	out.Before = state.level
	out.After = state.level

	if reading.Sequence == 0 || reading.ObservedAt.IsZero() {
		out.Ignored, out.IgnoreCause = true, "invalid_reading"
		return out
	}
	if reading.Sequence < state.latestSeq {
		out.Ignored, out.IgnoreCause = true, "out_of_order"
		return out
	}
	if !state.latestAt.IsZero() && reading.ObservedAt.Before(state.latestAt) {
		out.Ignored, out.IgnoreCause = true, "stale_observation"
		return out
	}
	raiseOnly := reading.Sequence == state.latestSeq

	capacity := p.capacity[reading.BasinID]
	ratio := clampRatio(reading.VolumeM3 / capacity)
	basis := p.buildBasis(reading, capacity, ratio, assignedFlow)
	previous := state.level
	next := p.resolve(previous, basis, raiseOnly)

	// Sequence/observation time always advance for accepted telegrams so a
	// later reordered reading is still rejected, but the audit basis is only
	// replaced by evidence at least as severe.
	state.latestSeq = reading.Sequence
	state.latestAt = reading.ObservedAt.UTC()
	state.fillRatio = ratio
	if state.basis == nil || rank(next) > rank(previous) ||
		basis.LevelRatio >= state.basis.LevelRatio || next == LevelNormal {
		state.basis = basis
	}
	if next != previous {
		state.activeSince = now.UTC()
	}
	state.level = next
	state.allowedFlow = p.computeAllowed(state, reading.BasinID, assignedFlow)

	out.Accepted, out.After, out.AllowedFlow, out.Basis = true, next, state.allowedFlow, state.basis
	return out
}

// resolve applies trigger thresholds upward and release (hysteresis)
// thresholds downward. De-escalation requires fresh evidence clearing the
// current band's release threshold AND the projection; it may walk through
// several bands in one reading.
func (p *Protection) resolve(current ProtectionLevel, basis *TriggerBasis, raiseOnly bool) ProtectionLevel {
	t := p.thresholds
	level, projected := basis.LevelRatio, basis.ProjectedRatio
	reaches := func(trigger float64) bool { return level >= trigger || projected >= trigger }

	target := LevelNormal
	switch {
	case reaches(t.StopAt):
		target = LevelStopped
	case reaches(t.RestrictAt):
		target = LevelRestricted
	case reaches(t.WarnAt):
		target = LevelWarning
	}
	// A retransmission can never lower protection.
	if raiseOnly && rank(target) < rank(current) {
		return current
	}
	if rank(target) >= rank(current) {
		return target
	}

	below := func(threshold float64) bool { return level < threshold && projected < threshold }
	switch current {
	case LevelStopped:
		switch {
		case !below(t.ReleaseStoppedAt):
			return LevelStopped
		case !below(t.ReleaseRestrictedAt):
			return LevelRestricted
		case !below(t.WarnAt):
			return LevelWarning
		default:
			return LevelNormal
		}
	case LevelRestricted:
		switch {
		case !below(t.ReleaseRestrictedAt):
			return LevelRestricted
		case !below(t.WarnAt):
			return LevelWarning
		default:
			return LevelNormal
		}
	case LevelWarning:
		if below(t.WarnAt) {
			return LevelNormal
		}
		return LevelWarning
	default:
		return LevelNormal
	}
}

func (p *Protection) buildBasis(reading Reading, capacity, ratio, assignedFlow float64) *TriggerBasis {
	t := p.thresholds
	projected := reading.VolumeM3 + assignedFlow*t.ProjectionHours
	projectedRatio := clampRatio(projected / capacity)
	basis := &TriggerBasis{
		Level: reading.VolumeM3, LevelRatio: ratio, Capacity: capacity,
		AssignedFlow: assignedFlow, Projected: projected, ProjectedRatio: projectedRatio,
		Sequence: reading.Sequence, ObservedAt: reading.ObservedAt.UTC(),
	}
	switch {
	case ratio >= t.StopAt:
		basis.Code, basis.TriggeredBy, basis.Threshold = "OVERFLOW_LEVEL_STOP", "level", t.StopAt
	case projectedRatio >= t.StopAt:
		basis.Code, basis.TriggeredBy, basis.Threshold = "OVERFLOW_PROJECTED_STOP", "projection", t.StopAt
	case ratio >= t.RestrictAt:
		basis.Code, basis.TriggeredBy, basis.Threshold = "OVERFLOW_LEVEL_RESTRICT", "level", t.RestrictAt
	case projectedRatio >= t.RestrictAt:
		basis.Code, basis.TriggeredBy, basis.Threshold = "OVERFLOW_PROJECTED_RESTRICT", "projection", t.RestrictAt
	case ratio >= t.WarnAt:
		basis.Code, basis.TriggeredBy, basis.Threshold = "OVERFLOW_WARNING", "level", t.WarnAt
	case projectedRatio >= t.WarnAt:
		basis.Code, basis.TriggeredBy, basis.Threshold = "OVERFLOW_WARNING", "projection", t.WarnAt
	default:
		basis.Code, basis.TriggeredBy = "OVERFLOW_CLEAR", "level"
	}
	return basis
}

// computeAllowed limits inflow to the smaller of the restricted-flow fraction
// and the rate that keeps the projected fill below the stop trigger. Stopped
// admits nothing.
func (p *Protection) computeAllowed(state *basinProtection, basinID string, assigned float64) float64 {
	if state.level != LevelRestricted {
		if state.level == LevelStopped {
			return 0
		}
		return assigned
	}
	headroom := p.thresholds.StopAt*p.capacity[basinID] - state.fillRatio*p.capacity[basinID]
	budget := headroom / p.thresholds.ProjectionHours
	fractionCap := assigned * p.thresholds.RestrictedFlowFraction
	capped := fractionCap
	if budget < capped {
		capped = budget
	}
	if capped < 0 {
		capped = 0
	}
	if assigned > 0 && capped > assigned {
		return assigned
	}
	return capped
}

// FlowDecision is the protection verdict for a proposed inflow.
type FlowDecision struct {
	Level       ProtectionLevel
	AllowedFlow float64
	Reason      string
}

// PermitFlow checks a proposed inflow for a basin against the current level.
// Warning and normal admit any flow; restricted caps it to the remaining
// headroom; stopped rejects all inflow. It does not mutate state.
func (p *Protection) PermitFlow(basinID string, flow float64) (FlowDecision, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.basins[basinID]
	if !ok {
		return FlowDecision{}, false
	}
	switch state.level {
	case LevelStopped:
		return FlowDecision{
			Level: LevelStopped, AllowedFlow: 0,
			Reason: fmt.Sprintf("basin %s is in overflow stop protection (%.1f%% full)", basinID, state.fillRatio*100),
		}, true
	case LevelRestricted:
		cap := p.computeAllowed(state, basinID, flow)
		if flow > cap {
			return FlowDecision{
				Level: LevelRestricted, AllowedFlow: cap,
				Reason: fmt.Sprintf("basin %s restricts intake to %.1f m3/h to stay under the overflow limit", basinID, cap),
			}, true
		}
		return FlowDecision{Level: LevelRestricted, AllowedFlow: flow}, true
	default:
		return FlowDecision{Level: state.level, AllowedFlow: flow}, true
	}
}

// RecomputeAllowed refreshes the flow cap and projection for a basin when its
// line assignment changes, without changing the protection level.
func (p *Protection) RecomputeAllowed(basinID string, assignedFlow float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.basins[basinID]
	if !ok || state.basis == nil {
		return
	}
	state.basis.AssignedFlow = assignedFlow
	state.basis.Projected = state.basis.Level + assignedFlow*p.thresholds.ProjectionHours
	state.basis.ProjectedRatio = clampRatio(state.basis.Projected / state.basis.Capacity)
	state.allowedFlow = p.computeAllowed(state, basinID, assignedFlow)
}

// Status returns the operator-facing protection status for a basin.
func (p *Protection) Status(basinID, lineID, batchID, alarmID string, now time.Time) (ProtectionStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.basins[basinID]
	if !ok {
		return ProtectionStatus{}, false
	}
	return ProtectionStatus{
		BasinID: basinID, LineID: lineID, BatchID: batchID, AlarmID: alarmID,
		Level: state.level, FillRatio: state.fillRatio,
		Headroom: p.capacity[basinID] * (1 - state.fillRatio),
		Basis:    state.basis, AllowedFlow: state.allowedFlow,
		UpdatedAt: state.latestAt, Recovery: p.recovery(state),
	}, true
}

func (p *Protection) recovery(state *basinProtection) RecoveryCondition {
	switch state.level {
	case LevelStopped:
		return RecoveryCondition{
			RequiredRatio: p.thresholds.ReleaseStoppedAt,
			Description: fmt.Sprintf(
				"需要新的水位样本：液位低于容量的 %.0f%%，且按当前进水流量预测 %.0f 小时内不超过该值",
				p.thresholds.ReleaseStoppedAt*100, p.thresholds.ProjectionHours),
			RequiresFresh: true, Since: state.activeSince,
		}
	case LevelRestricted:
		return RecoveryCondition{
			RequiredRatio: p.thresholds.ReleaseRestrictedAt,
			Description: fmt.Sprintf(
				"需要新的水位样本：液位低于容量的 %.0f%%，限流在此之前保持",
				p.thresholds.ReleaseRestrictedAt*100),
			RequiresFresh: true, Since: state.activeSince,
		}
	case LevelWarning:
		return RecoveryCondition{
			RequiredRatio: p.thresholds.WarnAt,
			Description:   fmt.Sprintf("新的水位样本显示液位低于容量的 %.0f%% 时自动解除预警", p.thresholds.WarnAt*100),
		}
	default:
		return RecoveryCondition{}
	}
}

// Links binds a basin to its current line / batch / alarm for a status report.
type Links struct {
	LineID  string
	BatchID string
	AlarmID string
}

// All returns statuses for every basin, sorted by id.
func (p *Protection) All(links map[string]Links, now time.Time) []ProtectionStatus {
	p.mu.Lock()
	ids := make([]string, 0, len(p.basins))
	for id := range p.basins {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	sort.Strings(ids)
	result := make([]ProtectionStatus, 0, len(ids))
	for _, id := range ids {
		link := links[id]
		if status, ok := p.Status(id, link.LineID, link.BatchID, link.AlarmID, now); ok {
			result = append(result, status)
		}
	}
	return result
}

// LevelOf returns the current protection level for a basin.
func (p *Protection) LevelOf(basinID string) (ProtectionLevel, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.basins[basinID]
	if !ok {
		return "", false
	}
	return state.level, true
}

func rank(level ProtectionLevel) int {
	switch level {
	case LevelStopped:
		return 3
	case LevelRestricted:
		return 2
	case LevelWarning:
		return 1
	default:
		return 0
	}
}

func clampRatio(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

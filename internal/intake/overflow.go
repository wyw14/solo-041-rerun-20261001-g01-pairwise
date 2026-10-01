package intake

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// ProtectionLevel is the latched overflow protection state of one basin.
type ProtectionLevel string

const (
	LevelNormal   ProtectionLevel = "normal"
	LevelWarning  ProtectionLevel = "warning"
	LevelRestrict ProtectionLevel = "restrict_inflow"
	LevelStop     ProtectionLevel = "stop_inflow"
)

// severityRank orders protection levels so independent trigger rules can be
// combined by taking the most severe one.
func severityRank(level ProtectionLevel) int {
	switch level {
	case LevelStop:
		return 3
	case LevelRestrict:
		return 2
	case LevelWarning:
		return 1
	default:
		return 0
	}
}

// Thresholds tunes the overflow protection. Level rules compare the measured
// level against the basin full scale; time rules compare the hydraulic time to
// overflow, derived from freeboard volume and the assigned inflow.
type Thresholds struct {
	WarningRatio       float64
	RestrictRatio      float64
	StopRatio          float64
	WarningHorizon     time.Duration
	RestrictHorizon    time.Duration
	StopHorizon        time.Duration
	RestrictFlowFactor float64
	RecoveryRatio      float64
	MaxSampleAge       time.Duration
	MinRecoveryDwell   time.Duration
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		WarningRatio:       0.80,
		RestrictRatio:      0.90,
		StopRatio:          0.95,
		WarningHorizon:     60 * time.Minute,
		RestrictHorizon:    30 * time.Minute,
		StopHorizon:        15 * time.Minute,
		RestrictFlowFactor: 0.5,
		RecoveryRatio:      0.70,
		MaxSampleAge:       10 * time.Minute,
		MinRecoveryDwell:   30 * time.Second,
	}
}

func (t Thresholds) validate() error {
	if !(0 < t.WarningRatio && t.WarningRatio < t.RestrictRatio && t.RestrictRatio < t.StopRatio && t.StopRatio <= 1) {
		return errors.New("overflow level ratios must satisfy 0 < warning < restrict < stop <= 1")
	}
	if !(0 < t.RecoveryRatio && t.RecoveryRatio < t.WarningRatio) {
		return errors.New("overflow recovery ratio must sit below the warning ratio")
	}
	if t.WarningHorizon <= 0 || t.RestrictHorizon <= 0 || t.StopHorizon <= 0 {
		return errors.New("overflow time horizons must be positive")
	}
	if t.StopHorizon > t.RestrictHorizon || t.RestrictHorizon > t.WarningHorizon {
		return errors.New("overflow time horizons must satisfy stop <= restrict <= warning")
	}
	if t.RestrictFlowFactor <= 0 || t.RestrictFlowFactor >= 1 {
		return errors.New("restricted flow factor must be between 0 and 1")
	}
	if t.MaxSampleAge <= 0 || t.MinRecoveryDwell < 0 {
		return errors.New("overflow sample age and recovery dwell must be positive")
	}
	return nil
}

// ProtectionBasis records why a protection level was engaged, so operators can
// inspect the evidence that triggered the action.
type ProtectionBasis struct {
	Rule                  string    `json:"rule"`
	WaterLevelM           float64   `json:"water_level_m"`
	FullLevelM            float64   `json:"full_level_m"`
	FillRatio             float64   `json:"fill_ratio"`
	AssignedFlowM3Hour    float64   `json:"assigned_flow_m3_hour"`
	FreeboardM3           float64   `json:"freeboard_m3"`
	TimeToOverflowMinutes *float64  `json:"time_to_overflow_minutes,omitempty"`
	ThresholdRatio        float64   `json:"threshold_ratio,omitempty"`
	ThresholdMinutes      float64   `json:"threshold_minutes,omitempty"`
	SampleSequence        uint64    `json:"sample_sequence"`
	ObservedAt            time.Time `json:"observed_at"`
}

// RecoveryCondition explains what must hold before protection is released.
type RecoveryCondition struct {
	Message                      string   `json:"message"`
	CurrentFillRatio             float64  `json:"current_fill_ratio"`
	RequireFillBelowRatio        float64  `json:"require_fill_below_ratio"`
	CurrentLevelM                float64  `json:"current_level_m"`
	RequireLevelBelowM           float64  `json:"require_level_below_m"`
	CurrentTimeToOverflowMinutes *float64 `json:"current_time_to_overflow_minutes,omitempty"`
	RequireTimeAboveMinutes      float64  `json:"require_time_to_overflow_above_minutes"`
	RequiresFreshSample          bool     `json:"requires_fresh_sample"`
	SampleStale                  bool     `json:"sample_stale"`
	DwellRequired                string   `json:"dwell_required,omitempty"`
	DwellRemaining               string   `json:"dwell_remaining,omitempty"`
	Satisfied                    bool     `json:"satisfied"`
}

// ProtectionView is the operator-facing state for one equalization basin.
type ProtectionView struct {
	BasinID               string             `json:"basin_id"`
	LineID                model.LineID       `json:"line_id,omitempty"`
	BatchID               uuid.UUID          `json:"batch_id,omitempty"`
	Level                 ProtectionLevel    `json:"level"`
	CapacityM3            float64            `json:"capacity_m3"`
	FullLevelM            float64            `json:"full_level_m"`
	WaterLevelM           float64            `json:"water_level_m"`
	FillRatio             float64            `json:"fill_ratio"`
	AssignedFlowM3Hour    float64            `json:"assigned_flow_m3_hour"`
	AllowedFlowM3Hour     float64            `json:"allowed_flow_m3_hour,omitempty"`
	TimeToOverflowMinutes *float64           `json:"time_to_overflow_minutes,omitempty"`
	SampleStale           bool               `json:"sample_stale"`
	Basis                 *ProtectionBasis   `json:"basis,omitempty"`
	Recovery              *RecoveryCondition `json:"recovery,omitempty"`
	AlarmIDs              []uuid.UUID        `json:"alarm_ids,omitempty"`
	ActivatedAt           time.Time          `json:"activated_at,omitempty"`
	UpdatedAt             time.Time          `json:"updated_at"`
}

// ProtectionNotice is emitted on every accepted transition, including release
// back to normal. Actions target the line/basin captured in the notice.
type ProtectionNotice struct {
	BasinID  string
	LineID   model.LineID
	BatchID  uuid.UUID
	Before   ProtectionLevel
	After    ProtectionLevel
	View     ProtectionView
	Released []uuid.UUID
}

// Notifier receives protection transitions. Implementations raise alarms,
// publish device commands or journal the change.
type Notifier interface {
	ProtectionChanged(ProtectionNotice)
}

// BatchResolver lets the guard associate an engagement with the active batch
// without depending on the process package.
type BatchResolver interface {
	ActiveBatchID(model.LineID) (uuid.UUID, bool)
}

type OverflowGuard struct {
	mu         sync.Mutex
	pool       *EqualizationPool
	thresholds Thresholds
	batches    BatchResolver
	notifier   Notifier
	now        func() time.Time
	states     map[string]*basinProtection
}

type basinProtection struct {
	level         ProtectionLevel
	last          model.Sample
	activatedAt   time.Time
	basis         *ProtectionBasis
	recoverySince time.Time
	lineID        model.LineID
	batchID       uuid.UUID
	alarms        []uuid.UUID
}

func NewOverflowGuard(pool *EqualizationPool, thresholds Thresholds, now func() time.Time) (*OverflowGuard, error) {
	if pool == nil {
		return nil, errors.New("overflow guard requires an equalization pool")
	}
	if now == nil {
		return nil, errors.New("overflow guard requires a clock")
	}
	if err := thresholds.validate(); err != nil {
		return nil, err
	}
	return &OverflowGuard{
		pool: pool, thresholds: thresholds, now: func() time.Time { return now().UTC() },
		states: make(map[string]*basinProtection),
	}, nil
}

// Bind wires the collaborators that associate actions with lines and batches.
func (g *OverflowGuard) Bind(batches BatchResolver, notifier Notifier) {
	g.mu.Lock()
	g.batches = batches
	g.notifier = notifier
	g.mu.Unlock()
}

// AttachAlarm records an alarm raised for an active engagement so it can be
// recovered automatically when protection releases.
func (g *OverflowGuard) AttachAlarm(basinID string, alarmID uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if state, ok := g.states[basinID]; ok && state.level != LevelNormal {
		state.alarms = append(state.alarms, alarmID)
	}
}

// Acceptable reports whether a water level sample is newer than the last one
// applied for its basin. It mutates nothing, so callers can reject expired or
// out-of-order readings before they are journaled.
func (g *OverflowGuard) Acceptable(sample model.Sample) error {
	if sample.Kind != model.SampleWaterLevel {
		return errors.New("overflow guard only accepts water level samples")
	}
	if !sample.Finite() || sample.Value < 0 || sample.ObservedAt.IsZero() {
		return errors.New("water level sample is invalid")
	}
	basin, ok := g.pool.find(sample.BasinID)
	if !ok {
		return fmt.Errorf("overflow guard does not monitor basin %q", sample.BasinID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.states[basin.ID]
	if state == nil {
		return nil
	}
	return g.checkOrderLocked(sample, state)
}

// checkOrderLocked returns an error when a sample is not newer than the last
// applied reading. Caller holds the guard mutex.
func (g *OverflowGuard) checkOrderLocked(sample model.Sample, state *basinProtection) error {
	if state.last.ObservedAt.IsZero() {
		return nil
	}
	switch {
	case sample.Sequence < state.last.Sequence:
		return fmt.Errorf("water level sequence %d is older than applied %d", sample.Sequence, state.last.Sequence)
	case sample.Sequence == state.last.Sequence:
		return fmt.Errorf("water level sequence %d was already applied", sample.Sequence)
	case !sample.ObservedAt.After(state.last.ObservedAt):
		return errors.New("water level observation is older than the latest reading")
	}
	return nil
}

// Ingest applies a water level sample reported for a basin. Expired samples
// (older observation timestamp) and out-of-order samples (non-monotonic
// sequence) are rejected and can never release protection.
func (g *OverflowGuard) Ingest(sample model.Sample) (ProtectionView, bool, error) {
	if sample.Kind != model.SampleWaterLevel {
		return ProtectionView{}, false, errors.New("overflow guard only accepts water level samples")
	}
	if !sample.Finite() || sample.Value < 0 || sample.ObservedAt.IsZero() {
		return ProtectionView{}, false, errors.New("water level sample is invalid")
	}
	basin, ok := g.pool.find(sample.BasinID)
	if !ok {
		return ProtectionView{}, false, fmt.Errorf("overflow guard does not monitor basin %q", sample.BasinID)
	}

	g.mu.Lock()
	state := g.states[basin.ID]
	if state == nil {
		state = &basinProtection{level: LevelNormal}
		g.states[basin.ID] = state
	}
	if err := g.checkOrderLocked(sample, state); err != nil {
		view := g.buildViewLocked(basin, state)
		g.mu.Unlock()
		return view, false, nil
	}

	updated, err := g.pool.UpdateLevel(basin.ID, sample.Value, sample.Sequence, sample.ObservedAt)
	if err != nil {
		g.mu.Unlock()
		return ProtectionView{}, false, err
	}
	state.last = sample
	notice := g.evaluateLocked(updated, state)
	view := g.buildViewLocked(updated, state)
	notifier := g.notifier
	g.mu.Unlock()

	if notice != nil && notifier != nil {
		notifier.ProtectionChanged(*notice)
	}
	return view, true, nil
}

// DetachLine clears the line/batch association for every basin assigned to the
// line (e.g. after an emergency isolation). Measurement history and latched
// protection are retained per basin so the protection cannot be reset by a
// line event.
func (g *OverflowGuard) DetachLine(lineID model.LineID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, basin := range g.pool.Snapshot() {
		if basin.LineID != lineID {
			continue
		}
		if state, ok := g.states[basin.ID]; ok {
			state.lineID = ""
			state.batchID = uuid.Nil
		}
	}
}

// Restore rehydrates a latched engagement from a journaled event during
// recovery. It does not notify or re-evaluate; the journaled view is the
// authority. Notifications stay suppressed to avoid duplicate alarms on replay.
func (g *OverflowGuard) Restore(basinID string, view ProtectionView, observedAt time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.pool.find(basinID); !ok {
		return fmt.Errorf("overflow guard does not monitor basin %q", basinID)
	}
	if _, err := g.pool.UpdateLevel(basinID, view.WaterLevelM, 0, observedAt); err != nil {
		return err
	}
	state := &basinProtection{
		level: view.Level, last: model.Sample{
			BasinID: basinID, Kind: model.SampleWaterLevel,
			Value: view.WaterLevelM, ObservedAt: observedAt,
		},
		activatedAt: view.ActivatedAt, basis: view.Basis,
		lineID: view.LineID, batchID: view.BatchID,
		alarms: append([]uuid.UUID(nil), view.AlarmIDs...),
	}
	g.states[basinID] = state
	return nil
}

// Reset clears a latched protection after operator intervention and
// re-evaluates the basin from its latest reading. A basin that is still above
// the recovery condition re-engages immediately.
func (g *OverflowGuard) Reset(basinID string) (ProtectionView, error) {
	g.mu.Lock()
	basin, ok := g.pool.find(basinID)
	if !ok {
		g.mu.Unlock()
		return ProtectionView{}, errors.New("equalizer is not found")
	}
	state := g.states[basinID]
	if state == nil {
		state = &basinProtection{level: LevelNormal}
		g.states[basinID] = state
	}
	released := append([]uuid.UUID(nil), state.alarms...)
	before := state.level
	state.level = LevelNormal
	state.basis = nil
	state.activatedAt = time.Time{}
	state.recoverySince = time.Time{}
	state.alarms = nil
	state.lineID = ""
	state.batchID = uuid.Nil
	var releaseNotice *ProtectionNotice
	if before != LevelNormal {
		releaseNotice = &ProtectionNotice{
			BasinID: basin.ID, Before: before, After: LevelNormal, Released: released,
			View: g.buildViewLocked(basin, state),
		}
	}
	// Re-engage immediately if the current reading is still unsafe.
	reEngage := g.evaluateLocked(basin, state)
	view := g.buildViewLocked(basin, state)
	notifier := g.notifier
	g.mu.Unlock()
	if notifier != nil {
		if releaseNotice != nil {
			notifier.ProtectionChanged(*releaseNotice)
		}
		if reEngage != nil {
			notifier.ProtectionChanged(*reEngage)
		}
	}
	return view, nil
}

// AdmissionDecision is the guard's verdict for a proposed intake admission.
type AdmissionDecision struct {
	BasinID      string
	Allowed      bool
	AdjustedFlow float64
	View         ProtectionView
	Reason       string
}

// Admission resolves the basin that would serve the line and applies its
// current protection. Protection only ever applies to the resolved basin, so a
// stop on one equalizer can never block intake routed to another.
func (g *OverflowGuard) Admission(lineID model.LineID, requestedFlow float64) (AdmissionDecision, error) {
	basin, err := g.pool.Resolve(lineID, requestedFlow)
	if err != nil {
		return AdmissionDecision{}, err
	}
	g.mu.Lock()
	state := g.states[basin.ID]
	view := g.buildViewLocked(basin, state)
	g.mu.Unlock()
	decision := AdmissionDecision{BasinID: basin.ID, Allowed: true, AdjustedFlow: requestedFlow, View: view}
	switch view.Level {
	case LevelStop:
		decision.Allowed = false
		decision.Reason = "inlet stopped: " + recoverySummary(view.Recovery)
	case LevelRestrict:
		decision.AdjustedFlow = requestedFlow * g.thresholds.RestrictFlowFactor
		decision.Reason = fmt.Sprintf("inflow restricted to %.0f%%", g.thresholds.RestrictFlowFactor*100)
	}
	return decision, nil
}

func recoverySummary(condition *RecoveryCondition) string {
	if condition == nil {
		return "overflow protection active"
	}
	return condition.Message
}

// View returns the operator-facing protection state for one basin.
func (g *OverflowGuard) View(basinID string) (ProtectionView, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	basin, ok := g.pool.find(basinID)
	if !ok {
		return ProtectionView{}, false
	}
	return g.buildViewLocked(basin, g.states[basinID]), true
}

// LineView returns the protection state of the basin assigned to a line.
func (g *OverflowGuard) LineView(lineID model.LineID) (ProtectionView, bool) {
	basin, ok := g.pool.FindByLine(lineID)
	if !ok {
		return ProtectionView{}, false
	}
	return g.View(basin.ID)
}

// Views returns protection state for every monitored basin.
func (g *OverflowGuard) Views() []ProtectionView {
	g.mu.Lock()
	defer g.mu.Unlock()
	basins := g.pool.Snapshot()
	result := make([]ProtectionView, 0, len(basins))
	for _, basin := range basins {
		result = append(result, g.buildViewLocked(basin, g.states[basin.ID]))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].BasinID < result[j].BasinID })
	return result
}

type levelMetrics struct {
	ratio          float64
	freeboardM3    float64
	inflow         float64
	timeToOverflow time.Duration
	hasInflow      bool
}

func measure(basin Equalizer) levelMetrics {
	m := levelMetrics{ratio: basin.WaterLevelM / basin.FullLevelM, inflow: basin.AssignedFlow}
	if m.ratio < 0 {
		m.ratio = 0
	}
	m.freeboardM3 = basin.Capacity * (1 - m.ratio)
	if m.freeboardM3 < 0 {
		m.freeboardM3 = 0
	}
	m.hasInflow = basin.AssignedFlow > 0
	if m.hasInflow {
		m.timeToOverflow = time.Duration(m.freeboardM3 / basin.AssignedFlow * float64(time.Hour))
	}
	return m
}

// evaluateLocked advances the state machine and returns a notice when the
// latched level changed. Caller holds the guard mutex.
func (g *OverflowGuard) evaluateLocked(basin Equalizer, state *basinProtection) *ProtectionNotice {
	now := g.now()
	t := g.thresholds
	m := measure(basin)
	desired, rule, ruleThreshold := g.desiredLevelLocked(t, m)

	before := state.level
	after := before
	switch {
	case severityRank(desired) > severityRank(before):
		after = desired
	case desired != before:
		// Conditions improved: release only when the recovery criteria hold,
		// otherwise the latch keeps the protection engaged.
		status := g.evaluateRecoveryLocked(t, basin, state, m, now)
		if status.ready {
			after = LevelNormal
		} else if !status.criteriaMet {
			state.recoverySince = time.Time{}
		}
	}
	if after == before {
		return nil
	}

	notice := &ProtectionNotice{BasinID: basin.ID, Before: before, After: after}
	if after == LevelNormal {
		state.level = LevelNormal
		state.basis = nil
		state.activatedAt = time.Time{}
		state.recoverySince = time.Time{}
		notice.Released = append([]uuid.UUID(nil), state.alarms...)
		state.alarms = nil
		state.lineID = ""
		state.batchID = uuid.Nil
		notice.View = g.buildViewLocked(basin, state)
		return notice
	}

	state.level = after
	state.basis = g.buildBasis(basin, m, rule, ruleThreshold, state.last)
	state.activatedAt = now
	state.recoverySince = time.Time{}
	state.lineID = basin.LineID
	if basin.LineID != "" && g.batches != nil {
		if batchID, ok := g.batches.ActiveBatchID(basin.LineID); ok {
			state.batchID = batchID
		}
	}
	notice.LineID = state.lineID
	notice.BatchID = state.batchID
	notice.View = g.buildViewLocked(basin, state)
	return notice
}

func (g *OverflowGuard) desiredLevelLocked(t Thresholds, m levelMetrics) (ProtectionLevel, string, float64) {
	level := LevelNormal
	rule := ""
	threshold := 0.0
	switch {
	case m.ratio >= t.StopRatio:
		level, rule, threshold = LevelStop, "fill_ratio", t.StopRatio
	case m.ratio >= t.RestrictRatio:
		level, rule, threshold = LevelRestrict, "fill_ratio", t.RestrictRatio
	case m.ratio >= t.WarningRatio:
		level, rule, threshold = LevelWarning, "fill_ratio", t.WarningRatio
	}
	if m.hasInflow {
		timeLevel := LevelNormal
		switch {
		case m.ratio >= 1 || m.timeToOverflow <= t.StopHorizon:
			timeLevel = LevelStop
		case m.timeToOverflow <= t.RestrictHorizon:
			timeLevel = LevelRestrict
		case m.timeToOverflow <= t.WarningHorizon:
			timeLevel = LevelWarning
		}
		if severityRank(timeLevel) > severityRank(level) {
			level = timeLevel
			rule = "time_to_overflow"
			switch timeLevel {
			case LevelStop:
				threshold = t.StopHorizon.Minutes()
			case LevelRestrict:
				threshold = t.RestrictHorizon.Minutes()
			case LevelWarning:
				threshold = t.WarningHorizon.Minutes()
			}
		}
	}
	return level, rule, threshold
}

func (g *OverflowGuard) buildBasis(basin Equalizer, m levelMetrics, rule string, ruleThreshold float64, sample model.Sample) *ProtectionBasis {
	basis := &ProtectionBasis{
		Rule: rule, WaterLevelM: basin.WaterLevelM, FullLevelM: basin.FullLevelM,
		FillRatio: m.ratio, AssignedFlowM3Hour: basin.AssignedFlow, FreeboardM3: m.freeboardM3,
		ThresholdRatio: 0, SampleSequence: sample.Sequence, ObservedAt: sample.ObservedAt,
	}
	if m.hasInflow {
		minutes := m.timeToOverflow.Minutes()
		basis.TimeToOverflowMinutes = &minutes
	}
	if rule == "fill_ratio" {
		basis.ThresholdRatio = ruleThreshold
	} else {
		basis.ThresholdMinutes = ruleThreshold
	}
	return basis
}

type recoveryStatus struct {
	criteriaMet bool
	ready       bool
	stale       bool
	remaining   time.Duration
}

// recoveryCheckLocked computes the release criteria without mutating state.
func (g *OverflowGuard) recoveryCheckLocked(t Thresholds, basin Equalizer, state *basinProtection, m levelMetrics, now time.Time) (bool, bool, time.Duration) {
	fresh := !state.last.ObservedAt.IsZero() && now.Sub(state.last.ObservedAt) <= t.MaxSampleAge
	levelOK := m.ratio <= t.RecoveryRatio
	var timeOK bool
	switch {
	case !m.hasInflow:
		timeOK = true
	case m.ratio >= 1:
		timeOK = false
	default:
		timeOK = m.timeToOverflow > t.WarningHorizon
	}
	if !fresh || !levelOK || !timeOK {
		return false, !fresh, 0
	}
	remaining := t.MinRecoveryDwell
	if !state.recoverySince.IsZero() {
		remaining -= now.Sub(state.recoverySince)
		if remaining < 0 {
			remaining = 0
		}
	}
	return true, false, remaining
}

// evaluateRecoveryLocked starts the dwell timer the first time the release
// criteria are observed and reports whether the latch may clear now.
func (g *OverflowGuard) evaluateRecoveryLocked(t Thresholds, basin Equalizer, state *basinProtection, m levelMetrics, now time.Time) recoveryStatus {
	met, stale, remaining := g.recoveryCheckLocked(t, basin, state, m, now)
	if !met {
		return recoveryStatus{stale: stale}
	}
	if state.recoverySince.IsZero() {
		state.recoverySince = now
		remaining = t.MinRecoveryDwell
	}
	return recoveryStatus{criteriaMet: true, ready: remaining == 0, remaining: remaining}
}

func (g *OverflowGuard) buildViewLocked(basin Equalizer, state *basinProtection) ProtectionView {
	now := g.now()
	m := measure(basin)
	view := ProtectionView{
		BasinID: basin.ID, LineID: basin.LineID, CapacityM3: basin.Capacity,
		FullLevelM: basin.FullLevelM, WaterLevelM: basin.WaterLevelM, FillRatio: m.ratio,
		AssignedFlowM3Hour: basin.AssignedFlow, UpdatedAt: now,
	}
	if m.hasInflow {
		minutes := m.timeToOverflow.Minutes()
		view.TimeToOverflowMinutes = &minutes
	}
	if state != nil {
		view.Level = state.level
		view.Basis = state.basis
		view.BatchID = state.batchID
		view.ActivatedAt = state.activatedAt
		view.AlarmIDs = append([]uuid.UUID(nil), state.alarms...)
		if !state.last.ObservedAt.IsZero() && now.Sub(state.last.ObservedAt) > g.thresholds.MaxSampleAge {
			view.SampleStale = true
		}
	}
	if view.Level == "" {
		view.Level = LevelNormal
	}
	if view.Level == LevelRestrict {
		view.AllowedFlowM3Hour = basin.AssignedFlow * g.thresholds.RestrictFlowFactor
	}
	if view.Level != LevelNormal {
		view.Recovery = g.buildRecoveryLocked(basin, state, m, now)
	}
	return view
}

func (g *OverflowGuard) buildRecoveryLocked(basin Equalizer, state *basinProtection, m levelMetrics, now time.Time) *RecoveryCondition {
	t := g.thresholds
	levelBelow := basin.FullLevelM * t.RecoveryRatio
	minutes := t.WarningHorizon.Minutes()
	condition := &RecoveryCondition{
		Message:                 fmt.Sprintf("level below %.2f m (%.0f%%) and time to overflow above %.0f min on a fresh sample", levelBelow, t.RecoveryRatio*100, minutes),
		CurrentFillRatio:        m.ratio,
		RequireFillBelowRatio:   t.RecoveryRatio,
		CurrentLevelM:           basin.WaterLevelM,
		RequireLevelBelowM:      levelBelow,
		RequireTimeAboveMinutes: minutes,
		RequiresFreshSample:     true,
		DwellRequired:           t.MinRecoveryDwell.String(),
	}
	if m.hasInflow {
		current := m.timeToOverflow.Minutes()
		condition.CurrentTimeToOverflowMinutes = &current
	}
	if state != nil {
		met, stale, remaining := g.recoveryCheckLocked(t, basin, state, m, now)
		condition.SampleStale = stale
		if remaining > 0 {
			condition.DwellRemaining = remaining.String()
		}
		condition.Satisfied = met && remaining == 0
	}
	return condition
}

// find locates a basin without exposing pool internals.
func (p *EqualizationPool) find(basinID string) (Equalizer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	basin, ok := p.basins[basinID]
	return basin, ok
}

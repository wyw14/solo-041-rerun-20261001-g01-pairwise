package process

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/intake"
	"github.com/wyw14/cry-97/internal/model"
)

// OverflowAction is the journaled record of an overflow-protection transition.
type OverflowAction struct {
	BasinID     string                 `json:"basin_id"`
	LineID      model.LineID           `json:"line_id"`
	BatchID     string                 `json:"batch_id,omitempty"`
	AlarmID     string                 `json:"alarm_id,omitempty"`
	Before      intake.ProtectionLevel `json:"before"`
	After       intake.ProtectionLevel `json:"after"`
	AllowedFlow float64                `json:"allowed_flow"`
	Basis       *intake.TriggerBasis   `json:"basis,omitempty"`
	ChangedAt   time.Time              `json:"changed_at"`
}

type overflowLink struct {
	lineID  model.LineID
	alarmID string
	batchID string
}

// ObserveWaterLevel accepts a water_level sample keyed by basin id, updates the
// overflow-protection book, and keeps alarms, batch linkage and the intake
// valve command in step with the protection level. Protection is evaluated
// strictly per basin, so a high level in one equalizer never acts on another.
func (p *Plant) ObserveWaterLevel(ctx context.Context, sample model.Sample) error {
	if sample.Kind != model.SampleWaterLevel {
		return errors.New("sample is not a water level reading")
	}
	protection := p.intake.Protection()
	if protection == nil {
		return errors.New("overflow protection is not configured")
	}
	now := p.now()

	// Resolve the current line assignment for the reported basin. After a stop
	// the reservation is released, so fall back to the line the protection was
	// first raised for — that line stays linked until recovery.
	var lineID model.LineID
	var assignedFlow float64
	for _, basin := range p.equalizers.Snapshot() {
		if basin.ID == sample.BasinID {
			lineID = basin.LineID
			assignedFlow = basin.AssignedFlow
			break
		}
	}
	storedLink := p.overflowLinks[sample.BasinID]
	if lineID == "" && storedLink.lineID != "" {
		lineID = storedLink.lineID
	}
	reading := intake.Reading{
		BasinID: sample.BasinID, Sequence: sample.Sequence,
		VolumeM3: sample.Value, ObservedAt: sample.ObservedAt,
	}
	outcome := protection.Observe(reading, string(lineID), assignedFlow, now)
	if outcome.Ignored {
		// Stale, out-of-order or unknown-basin telegrams never change
		// protection; report them without acting.
		return fmt.Errorf("water level reading ignored: %s", outcome.IgnoreCause)
	}
	if outcome.Before == outcome.After && outcome.After == intake.LevelNormal {
		return nil
	}
	if outcome.Before == outcome.After {
		// Same band: refresh the valve setpoint (flow assignment may have
		// changed) but do not raise duplicate alarms.
		p.applyIntakeAction(lineID, sample.BasinID, outcome.After, outcome.AllowedFlow)
		p.publishIntakeCommand(ctx, sample.BasinID, lineID, outcome.After, outcome.AllowedFlow)
		return nil
	}

	link := storedLink
	if lineID != "" {
		link.lineID = lineID
	}
	if batch, ok := p.ActiveBatch(lineID); ok && lineID != "" {
		link.batchID = batch.ID.String()
	}

	// Recover the alarm from the band we are leaving.
	if link.alarmID != "" {
		if id, err := uuid.Parse(link.alarmID); err == nil {
			if _, err := p.alarms.Recover(id, now); err == nil {
				link.alarmID = ""
			}
		}
	}

	// Raise the alarm for the band we are entering and journal the action.
	if outcome.After != intake.LevelNormal {
		severity := model.SeverityWarning
		code := "INTAKE_OVERFLOW_WARNING"
		if outcome.After == intake.LevelRestricted {
			severity, code = model.SeverityCritical, "INTAKE_OVERFLOW_RESTRICT"
		}
		if outcome.After == intake.LevelStopped {
			severity, code = model.SeverityCritical, "INTAKE_OVERFLOW_STOP"
		}
		message := p.overflowMessage(sample.BasinID, lineID, outcome)
		alarm := p.alarms.Raise(model.NewAlarm(lineID, code, message, severity, now))
		link.alarmID = alarm.ID.String()
	}

	action := OverflowAction{
		BasinID: sample.BasinID, LineID: lineID, BatchID: link.batchID,
		AlarmID: link.alarmID, Before: outcome.Before, After: outcome.After,
		AllowedFlow: outcome.AllowedFlow, Basis: outcome.Basis, ChangedAt: now,
	}
	p.recordOverflowAction(ctx, lineID, action)
	if outcome.After == intake.LevelNormal {
		delete(p.overflowLinks, sample.BasinID)
	} else {
		p.overflowLinks[sample.BasinID] = link
	}
	p.applyIntakeAction(lineID, sample.BasinID, outcome.After, outcome.AllowedFlow)
	if outcome.After == intake.LevelStopped {
		p.equalizers.BlockBasin(sample.BasinID)
	}
	if outcome.Before == intake.LevelStopped && outcome.After != intake.LevelStopped {
		p.equalizers.UnblockBasin(sample.BasinID)
	}
	p.publishIntakeCommand(ctx, sample.BasinID, lineID, outcome.After, outcome.AllowedFlow)
	return nil
}

// applyIntakeAction translates the protection band into action on the running
// intake admission — but only when that admission is actually on the reported
// basin. After a stop releases the reservation, the line may have moved to
// another equalizer; acting again would wrongly stop that other basin.
func (p *Plant) applyIntakeAction(lineID model.LineID, basinID string, level intake.ProtectionLevel, allowedFlow float64) {
	if lineID == "" {
		return
	}
	if admission, ok := p.intake.Current(lineID); ok && admission.EqualizerID != basinID {
		return
	}
	switch level {
	case intake.LevelStopped:
		p.intake.CloseIntake(lineID)
	case intake.LevelRestricted:
		p.intake.Throttle(lineID, allowedFlow)
	}
}

func (p *Plant) recordOverflowAction(ctx context.Context, lineID model.LineID, action OverflowAction) {
	generation := uint64(1)
	if lineID != "" {
		p.mu.RLock()
		if state, ok := p.lines[lineID]; ok {
			generation = state.Generation
		}
		p.mu.RUnlock()
	}
	event, err := model.NewEvent(lineID, generation, model.EventIntakeProtected, action, action.ChangedAt)
	if err != nil {
		return
	}
	if _, err := p.store.Append(ctx, event); err != nil {
		return
	}
}

func (p *Plant) overflowMessage(basinID string, lineID model.LineID, outcome intake.Outcome) string {
	if outcome.Basis == nil {
		return fmt.Sprintf("equalizer %s overflow protection %s", basinID, outcome.After)
	}
	basis := outcome.Basis
	switch outcome.After {
	case intake.LevelStopped:
		return fmt.Sprintf("均衡池 %s（处理线 %s）液位 %.0f%%（%.0f/%.0f m³），触发溢流停止进水",
			basinID, lineID, basis.LevelRatio*100, basis.Level, basis.Capacity)
	case intake.LevelRestricted:
		return fmt.Sprintf("均衡池 %s（处理线 %s）液位 %.0f%%，按当前 %.0f m³/h 进水将逼近容量，已限流至 %.0f m³/h",
			basinID, lineID, basis.LevelRatio*100, basis.AssignedFlow, outcome.AllowedFlow)
	default:
		return fmt.Sprintf("均衡池 %s（处理线 %s）液位 %.0f%%，发出溢流预警",
			basinID, lineID, basis.LevelRatio*100)
	}
}

// publishIntakeCommand drives the basin inlet valve: throttle sets a flow cap,
// stop closes it, clearing re-opens it. The command targets the basin only, so
// valves on other equalizers are untouched.
func (p *Plant) publishIntakeCommand(ctx context.Context, basinID string, lineID model.LineID, level intake.ProtectionLevel, allowedFlow float64) {
	if lineID == "" {
		return
	}
	batch, ok := p.ActiveBatch(lineID)
	if !ok {
		return
	}
	p.mu.RLock()
	state := p.lines[lineID]
	p.mu.RUnlock()
	action, setpoint := "open", 0.0
	switch level {
	case intake.LevelStopped:
		action, setpoint = "close", 0
	case intake.LevelRestricted:
		action, setpoint = "throttle", allowedFlow
	}
	command, err := model.NewDeviceCommand(
		lineID, batch.ID, "intake-valve-"+basinID, model.DeviceValve,
		state.Generation, action, p.now())
	if err != nil {
		return
	}
	command.Setpoint = setpoint
	command.Metadata["basin_id"] = basinID
	command.Metadata["protection_level"] = string(level)
	_ = p.devices.Publish(ctx, command)
}

// ReportWaterLevel is the basin-addressed entry point for water-level
// telemetry ("按池编号上报"). The line is resolved from the current assignment
// or the protection link, so reports keep working after a stop released the
// reservation. Stale or out-of-order readings are rejected without clearing
// protection.
func (p *Plant) ReportWaterLevel(ctx context.Context, basinID string, sequence uint64, volumeM3 float64, unit string, observedAt time.Time) (intake.ProtectionStatus, error) {
	if basinID == "" {
		return intake.ProtectionStatus{}, errors.New("basin id is required")
	}
	if sequence == 0 {
		return intake.ProtectionStatus{}, errors.New("sample sequence is required")
	}
	if volumeM3 < 0 {
		return intake.ProtectionStatus{}, errors.New("water level volume cannot be negative")
	}
	if observedAt.IsZero() {
		observedAt = p.now()
	}
	var lineID model.LineID
	for _, basin := range p.equalizers.Snapshot() {
		if basin.ID == basinID {
			lineID = basin.LineID
			break
		}
	}
	if lineID == "" {
		if link, ok := p.overflowLinks[basinID]; ok {
			lineID = link.lineID
		}
	}
	sample := model.Sample{
		LineID: lineID, BasinID: basinID, Kind: model.SampleWaterLevel,
		Sequence: sequence, Value: volumeM3, Unit: unit, ObservedAt: observedAt.UTC(),
	}
	if err := p.ObserveWaterLevel(ctx, sample); err != nil {
		return intake.ProtectionStatus{}, err
	}
	return p.OverflowBasin(basinID)
}

// OverflowProtection reports the current protection status of every basin.
func (p *Plant) OverflowProtection() []intake.ProtectionStatus {
	protection := p.intake.Protection()
	if protection == nil {
		return nil
	}
	links := make(map[string]intake.Links)
	for _, basin := range p.equalizers.Snapshot() {
		link := intake.Links{LineID: string(basin.LineID)}
		stored, hasLink := p.overflowLinks[basin.ID]
		if link.LineID == "" && hasLink {
			link.LineID = string(stored.lineID)
		}
		if hasLink {
			link.AlarmID = stored.alarmID
			link.BatchID = stored.batchID
		}
		if basin.LineID != "" {
			if batch, ok := p.ActiveBatch(basin.LineID); ok {
				link.BatchID = batch.ID.String()
			}
		}
		links[basin.ID] = link
	}
	return protection.All(links, p.now())
}

// OverflowBasin reports the protection status of one basin.
func (p *Plant) OverflowBasin(basinID string) (intake.ProtectionStatus, error) {
	protection := p.intake.Protection()
	if protection == nil {
		return intake.ProtectionStatus{}, errors.New("overflow protection is not configured")
	}
	var lineID, batchID, alarmID string
	for _, basin := range p.equalizers.Snapshot() {
		if basin.ID == basinID {
			lineID = string(basin.LineID)
			if basin.LineID != "" {
				if batch, ok := p.ActiveBatch(basin.LineID); ok {
					batchID = batch.ID.String()
				}
			}
		}
	}
	if link, ok := p.overflowLinks[basinID]; ok {
		if lineID == "" {
			lineID = string(link.lineID)
		}
		alarmID = link.alarmID
		if batchID == "" {
			batchID = link.batchID
		}
	}
	status, ok := protection.Status(basinID, lineID, batchID, alarmID, p.now())
	if !ok {
		return intake.ProtectionStatus{}, errors.New("overflow protection basin is not found")
	}
	return status, nil
}

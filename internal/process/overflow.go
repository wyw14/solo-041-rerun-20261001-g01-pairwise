package process

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/intake"
	"github.com/wyw14/cry-97/internal/model"
)

// overflowEvent is the journal payload for a protection transition.
type overflowEvent struct {
	BasinID  string                 `json:"basin_id"`
	LineID   model.LineID           `json:"line_id,omitempty"`
	BatchID  uuid.UUID              `json:"batch_id,omitempty"`
	Before   intake.ProtectionLevel `json:"before"`
	After    intake.ProtectionLevel `json:"after"`
	View     intake.ProtectionView  `json:"view"`
	Released []uuid.UUID            `json:"released_alarm_ids,omitempty"`
}

// ActiveBatchID lets the overflow guard associate an engagement with the batch
// running on the line without the intake package depending on process.
func (p *Plant) ActiveBatchID(lineID model.LineID) (uuid.UUID, bool) {
	batch, ok := p.ActiveBatch(lineID)
	if !ok || !batch.IsActive() {
		return uuid.Nil, false
	}
	return batch.ID, true
}

// ProtectionChanged reacts to overflow protection transitions: it raises and
// recovers alarms tied to the active batch, and drives the basin inlet device.
// It only ever touches the line captured in the notice, so an engagement on
// one equalizer can never stop another basin.
func (p *Plant) ProtectionChanged(notice intake.ProtectionNotice) {
	now := p.now()
	for _, alarmID := range notice.Released {
		_, _ = p.alarms.Recover(alarmID, now)
	}

	var alarmID uuid.UUID
	if notice.After != intake.LevelNormal {
		alarm := p.raiseOverflowAlarm(notice, now)
		alarmID = alarm.ID
		p.overflow.AttachAlarm(notice.BasinID, alarmID)
	}
	p.publishOverflowCommand(notice, now)
	p.journalOverflow(notice, alarmID, now)
}

func (p *Plant) raiseOverflowAlarm(notice intake.ProtectionNotice, now time.Time) model.Alarm {
	view := notice.View
	lineID := notice.LineID
	var code string
	var severity model.AlarmSeverity
	var message string
	switch notice.After {
	case intake.LevelWarning:
		code = "OVERFLOW_LEVEL_WARNING"
		severity = model.SeverityWarning
		message = fmt.Sprintf("equalizer %s high water level %.2f/%.2f m (%.0f%%)",
			notice.BasinID, view.WaterLevelM, view.FullLevelM, view.FillRatio*100)
	case intake.LevelRestrict:
		code = "OVERFLOW_INFLOW_RESTRICTED"
		severity = model.SeverityWarning
		message = fmt.Sprintf("equalizer %s inflow restricted to %.0f m3/h, level %.0f%%",
			notice.BasinID, view.AllowedFlowM3Hour, view.FillRatio*100)
	case intake.LevelStop:
		code = "OVERFLOW_INFLOW_STOP"
		severity = model.SeverityCritical
		message = fmt.Sprintf("equalizer %s inlet stopped, level %.2f/%.2f m (%.0f%%), imminent overflow",
			notice.BasinID, view.WaterLevelM, view.FullLevelM, view.FillRatio*100)
	default:
		code = "OVERFLOW_PROTECTION"
		severity = model.SeverityWarning
		message = fmt.Sprintf("equalizer %s protection level %s", notice.BasinID, notice.After)
	}
	if notice.BatchID != uuid.Nil {
		message = fmt.Sprintf("%s [batch %s]", message, notice.BatchID.String()[:8])
	}
	return p.alarms.Raise(model.NewAlarm(lineID, code, message, severity, now))
}

func (p *Plant) publishOverflowCommand(notice intake.ProtectionNotice, now time.Time) {
	if notice.LineID == "" || notice.BatchID == uuid.Nil {
		return
	}
	generation, ok := p.lineGeneration(notice.LineID)
	if !ok {
		return
	}
	action := "set"
	setpoint := notice.View.AssignedFlowM3Hour
	switch notice.After {
	case intake.LevelStop:
		action = "stop"
		setpoint = 0
	case intake.LevelRestrict:
		action = "restrict"
		setpoint = notice.View.AllowedFlowM3Hour
	case intake.LevelNormal:
		action = "resume"
		setpoint = notice.View.AssignedFlowM3Hour
	default:
		return
	}
	command, err := model.NewDeviceCommand(
		notice.LineID, notice.BatchID, "inlet-"+notice.BasinID,
		model.DeviceValve, generation, action, now,
	)
	if err != nil {
		return
	}
	command.Setpoint = setpoint
	command.Metadata["basin_id"] = notice.BasinID
	command.Metadata["protection_level"] = string(notice.After)
	_ = p.devices.Publish(context.Background(), command)
}

func (p *Plant) journalOverflow(notice intake.ProtectionNotice, alarmID uuid.UUID, now time.Time) {
	if notice.LineID == "" {
		return
	}
	generation, ok := p.lineGeneration(notice.LineID)
	if !ok {
		return
	}
	payload := overflowEvent{
		BasinID: notice.BasinID, LineID: notice.LineID, BatchID: notice.BatchID,
		Before: notice.Before, After: notice.After, View: notice.View,
		Released: notice.Released,
	}
	if alarmID != uuid.Nil {
		payload.View.AlarmIDs = append(payload.View.AlarmIDs, alarmID)
	}
	event, err := model.NewEvent(notice.LineID, generation, model.EventOverflowChanged, payload, now)
	if err != nil {
		return
	}
	_, _ = p.store.Append(context.Background(), event)
}

func (p *Plant) lineGeneration(lineID model.LineID) (uint64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	state, ok := p.lines[lineID]
	if !ok {
		return 0, false
	}
	return state.Generation, true
}

// OverflowViews returns operator-facing protection state for every basin.
func (p *Plant) OverflowViews() []intake.ProtectionView {
	return p.overflow.Views()
}

// OverflowView returns the protection state for one basin.
func (p *Plant) OverflowView(basinID string) (intake.ProtectionView, bool) {
	return p.overflow.View(basinID)
}

// ResetOverflow clears a latched protection after operator intervention.
func (p *Plant) ResetOverflow(basinID string) (intake.ProtectionView, error) {
	return p.overflow.Reset(basinID)
}

// ingestWaterLevel feeds a reported water level into the overflow guard.
func (p *Plant) ingestWaterLevel(sample model.Sample) error {
	_, _, err := p.overflow.Ingest(sample)
	return err
}

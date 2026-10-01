package model

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type EventKind string

const (
	EventBatchCreated      EventKind = "batch.created"
	EventStageChanged      EventKind = "batch.stage_changed"
	EventSampleStored      EventKind = "sample.stored"
	EventDosingChanged     EventKind = "dosing.changed"
	EventDeviceCommanded   EventKind = "device.commanded"
	EventEmergencyIsolated EventKind = "emergency.isolated"
	EventOverflowChanged   EventKind = "overflow.protection_changed"
)

type Event struct {
	ID         uuid.UUID       `json:"id"`
	LineID     LineID          `json:"line_id"`
	Sequence   uint64          `json:"sequence"`
	Generation uint64          `json:"generation"`
	Kind       EventKind       `json:"kind"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

func NewEvent(lineID LineID, generation uint64, kind EventKind, value any, now time.Time) (Event, error) {
	if lineID == "" || generation == 0 || kind == "" {
		return Event{}, errors.New("event identity is incomplete")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID: uuid.New(), LineID: lineID, Generation: generation, Kind: kind,
		OccurredAt: now.UTC(), Payload: payload,
	}, nil
}

func (e Event) Decode(target any) error {
	if len(e.Payload) == 0 {
		return errors.New("event payload is empty")
	}
	return json.Unmarshal(e.Payload, target)
}

func (e Event) IsTerminal() bool {
	return e.Kind == EventEmergencyIsolated
}

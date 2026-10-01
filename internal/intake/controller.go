package intake

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

type Admission struct {
	LineID           model.LineID `json:"line_id"`
	RequestedFlowM3H float64      `json:"requested_flow_m3_hour"`
	FlowM3Hour       float64      `json:"flow_m3_hour"`
	Throttled        bool         `json:"throttled"`
	Source           string       `json:"source"`
	AcceptedAt       time.Time    `json:"accepted_at"`
	EqualizerID      string       `json:"equalizer_id"`
}

// FlowLimitedError reports that overflow protection rejected or capped an
// admission request.
type FlowLimitedError struct {
	EqualizerID string
	Level       ProtectionLevel
	AllowedFlow float64
	Reason      string
}

func (e *FlowLimitedError) Error() string {
	if e.Level == LevelStopped {
		return fmt.Sprintf("intake stopped by overflow protection on %s: %s", e.EqualizerID, e.Reason)
	}
	return fmt.Sprintf("intake restricted by overflow protection on %s: %s", e.EqualizerID, e.Reason)
}

type Controller struct {
	mu         sync.RWMutex
	capacity   map[model.LineID]float64
	admissions map[model.LineID]Admission
	equalizers *EqualizationPool
	protection *Protection
}

func NewController(equalizers *EqualizationPool) (*Controller, error) {
	if equalizers == nil {
		return nil, errors.New("equalization pool is required")
	}
	return &Controller{
		capacity: make(map[model.LineID]float64), admissions: make(map[model.LineID]Admission),
		equalizers: equalizers,
	}, nil
}

// BindProtection attaches the overflow-protection book. Once bound, every
// admission is checked against the assigned basin.
func (c *Controller) BindProtection(protection *Protection) {
	c.mu.Lock()
	c.protection = protection
	c.mu.Unlock()
}

func (c *Controller) Configure(lineID model.LineID, maximumFlow float64) error {
	if lineID == "" || maximumFlow <= 0 {
		return errors.New("line and positive intake capacity are required")
	}
	c.mu.Lock()
	c.capacity[lineID] = maximumFlow
	c.mu.Unlock()
	return nil
}

func (c *Controller) Admit(ctx context.Context, lineID model.LineID, flow float64, source string, now time.Time) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return Admission{}, err
	}
	c.mu.RLock()
	capacity, configured := c.capacity[lineID]
	protection := c.protection
	c.mu.RUnlock()
	if !configured {
		return Admission{}, errors.New("process line intake is not configured")
	}
	if flow <= 0 || flow > capacity {
		return Admission{}, errors.New("intake flow exceeds configured range")
	}
	// Route around free basins that are currently in stop protection; a
	// stopped basin is skipped so intake lands on another healthy equalizer
	// instead of failing the line outright.
	exclude := make(map[string]struct{})
	var basin Equalizer
	var lastDecision FlowDecision
	lastStopped := ""
	for {
		var err error
		basin, err = c.equalizers.AssignExcluding(lineID, flow, now, exclude)
		if err != nil {
			if len(exclude) > 0 {
				return Admission{}, &FlowLimitedError{
					EqualizerID: lastStopped, Level: LevelStopped,
					AllowedFlow: 0, Reason: lastDecision.Reason,
				}
			}
			return Admission{}, err
		}
		if protection == nil {
			break
		}
		decision, known := protection.PermitFlow(basin.ID, flow)
		lastDecision = decision
		if known && decision.Level == LevelStopped {
			if _, skipped := exclude[basin.ID]; skipped {
				c.equalizers.Release(lineID)
				return Admission{}, &FlowLimitedError{
					EqualizerID: basin.ID, Level: LevelStopped,
					AllowedFlow: 0, Reason: decision.Reason,
				}
			}
			lastStopped = basin.ID
			exclude[basin.ID] = struct{}{}
			c.equalizers.Release(lineID)
			continue
		}
		break
	}
	admittedFlow := flow
	throttled := false
	if protection != nil {
		decision, known := protection.PermitFlow(basin.ID, flow)
		if known && decision.Level == LevelStopped {
			// All candidate basins were in stop protection.
			c.equalizers.Release(lineID)
			return Admission{}, &FlowLimitedError{
				EqualizerID: basin.ID, Level: LevelStopped,
				AllowedFlow: 0, Reason: decision.Reason,
			}
		}
		if known && decision.Level == LevelRestricted && decision.AllowedFlow < flow {
			admittedFlow = decision.AllowedFlow
			throttled = true
			if updated, err := c.equalizers.Assign(lineID, admittedFlow, now); err != nil {
				c.equalizers.Release(lineID)
				return Admission{}, err
			} else {
				basin = updated
			}
		}
		protection.RecomputeAllowed(basin.ID, admittedFlow)
	}
	admission := Admission{
		LineID: lineID, RequestedFlowM3H: flow, FlowM3Hour: admittedFlow,
		Throttled: throttled, Source: source, AcceptedAt: now.UTC(), EqualizerID: basin.ID,
	}
	c.mu.Lock()
	c.admissions[lineID] = admission
	c.mu.Unlock()
	return admission, nil
}

func (c *Controller) Current(lineID model.LineID) (Admission, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.admissions[lineID]
	return value, ok
}

// Throttle lowers the admitted flow of a running admission to the new cap. It
// never raises flow and never touches other lines.
func (c *Controller) Throttle(lineID model.LineID, flow float64) (Admission, bool) {
	c.mu.Lock()
	admission, ok := c.admissions[lineID]
	if !ok || flow >= admission.FlowM3Hour {
		c.mu.Unlock()
		return admission, ok
	}
	admission.FlowM3Hour = flow
	admission.Throttled = true
	c.admissions[lineID] = admission
	c.mu.Unlock()
	c.equalizers.SetAssignedFlow(lineID, flow)
	return admission, true
}

// CloseIntake marks the running admission as stopped (zero admitted flow) and
// releases its equalizer reservation.
func (c *Controller) CloseIntake(lineID model.LineID) {
	c.mu.Lock()
	if admission, ok := c.admissions[lineID]; ok {
		admission.FlowM3Hour = 0
		admission.Throttled = true
		c.admissions[lineID] = admission
	}
	c.mu.Unlock()
	c.equalizers.Release(lineID)
}

// Protection exposes the bound protection book, if any.
func (c *Controller) Protection() *Protection {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.protection
}

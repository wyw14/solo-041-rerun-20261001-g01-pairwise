package intake

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

type Admission struct {
	LineID        model.LineID    `json:"line_id"`
	FlowM3Hour    float64         `json:"flow_m3_hour"`
	RequestedFlow float64         `json:"requested_flow_m3_hour,omitempty"`
	Source        string          `json:"source"`
	AcceptedAt    time.Time       `json:"accepted_at"`
	EqualizerID   string          `json:"equalizer_id"`
	Protection    ProtectionLevel `json:"protection,omitempty"`
	Note          string          `json:"note,omitempty"`
}

type Controller struct {
	mu         sync.RWMutex
	capacity   map[model.LineID]float64
	admissions map[model.LineID]Admission
	equalizers *EqualizationPool
	guard      *OverflowGuard
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

// BindGuard wires the overflow protection that gates every admission.
func (c *Controller) BindGuard(guard *OverflowGuard) {
	c.mu.Lock()
	c.guard = guard
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
	guard := c.guard
	c.mu.RUnlock()
	if !configured {
		return Admission{}, errors.New("process line intake is not configured")
	}
	if flow <= 0 || flow > capacity {
		return Admission{}, errors.New("intake flow exceeds configured range")
	}
	requested := flow
	note := ""
	protection := LevelNormal
	if guard != nil {
		decision, err := guard.Admission(lineID, flow)
		if err != nil {
			return Admission{}, err
		}
		if !decision.Allowed {
			return Admission{}, errors.New("intake blocked by equalizer overflow protection: " + decision.Reason)
		}
		flow = decision.AdjustedFlow
		protection = decision.View.Level
		note = decision.Reason
	}
	basin, err := c.equalizers.Assign(lineID, flow, now)
	if err != nil {
		return Admission{}, err
	}
	admission := Admission{
		LineID: lineID, FlowM3Hour: flow, RequestedFlow: requested, Source: source,
		AcceptedAt: now.UTC(), EqualizerID: basin.ID, Protection: protection, Note: note,
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

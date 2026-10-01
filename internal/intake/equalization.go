package intake

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

type Equalizer struct {
	ID           string       `json:"id"`
	Capacity     float64      `json:"capacity"`
	AssignedFlow float64      `json:"assigned_flow"`
	LineID       model.LineID `json:"line_id,omitempty"`
	AssignedAt   time.Time    `json:"assigned_at,omitempty"`
}

type EqualizationPool struct {
	mu     sync.Mutex
	basins map[string]Equalizer
	// blocked basin ids cannot accept new assignments; it is driven by
	// overflow protection and is independent of line reservations.
	blocked map[string]struct{}
}

func NewEqualizationPool(basins []Equalizer) (*EqualizationPool, error) {
	if len(basins) == 0 {
		return nil, errors.New("at least one equalizer is required")
	}
	pool := &EqualizationPool{
		basins:  make(map[string]Equalizer, len(basins)),
		blocked: make(map[string]struct{}),
	}
	for _, basin := range basins {
		if basin.ID == "" || basin.Capacity <= 0 {
			return nil, errors.New("equalizer identity and capacity are required")
		}
		pool.basins[basin.ID] = basin
	}
	return pool, nil
}

// BlockBasin prevents new line assignments to a basin (overflow stop). It does
// not touch an existing reservation; callers release that separately.
func (p *EqualizationPool) BlockBasin(id string) {
	p.mu.Lock()
	p.blocked[id] = struct{}{}
	p.mu.Unlock()
}

// UnblockBasin allows assignments to a basin again after protection clears.
func (p *EqualizationPool) UnblockBasin(id string) {
	p.mu.Lock()
	delete(p.blocked, id)
	p.mu.Unlock()
}

func (p *EqualizationPool) IsBlocked(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, blocked := p.blocked[id]
	return blocked
}

func (p *EqualizationPool) Assign(lineID model.LineID, flow float64, now time.Time) (Equalizer, error) {
	return p.AssignExcluding(lineID, flow, now, nil)
}

// AssignExcluding works like Assign but skips the given basin ids (used to
// route around basins currently in overflow stop).
func (p *EqualizationPool) AssignExcluding(lineID model.LineID, flow float64, now time.Time, exclude map[string]struct{}) (Equalizer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, basin := range p.basins {
		if basin.LineID == lineID {
			basin.AssignedFlow = flow
			basin.AssignedAt = now.UTC()
			p.basins[id] = basin
			return basin, nil
		}
	}
	ids := make([]string, 0, len(p.basins))
	for id := range p.basins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, blocked := p.blocked[id]; blocked {
			continue
		}
		if _, skipped := exclude[id]; skipped {
			continue
		}
		basin := p.basins[id]
		if basin.LineID == "" && flow <= basin.Capacity {
			basin.LineID = lineID
			basin.AssignedFlow = flow
			basin.AssignedAt = now.UTC()
			p.basins[id] = basin
			return basin, nil
		}
	}
	return Equalizer{}, errors.New("no equalizer can accept the requested flow")
}

func (p *EqualizationPool) Release(lineID model.LineID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, basin := range p.basins {
		if basin.LineID != lineID {
			continue
		}
		basin.LineID = ""
		basin.AssignedFlow = 0
		basin.AssignedAt = time.Time{}
		p.basins[id] = basin
	}
}

// SetAssignedFlow updates the flow of a line's current reservation (used when
// overflow protection throttles a running admission).
func (p *EqualizationPool) SetAssignedFlow(lineID model.LineID, flow float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, basin := range p.basins {
		if basin.LineID != lineID {
			continue
		}
		basin.AssignedFlow = flow
		p.basins[id] = basin
	}
}

func (p *EqualizationPool) Snapshot() []Equalizer {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]Equalizer, 0, len(p.basins))
	for _, basin := range p.basins {
		result = append(result, basin)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

package intake

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

type Equalizer struct {
	ID              string       `json:"id"`
	Capacity        float64      `json:"capacity"`
	AssignedFlow    float64      `json:"assigned_flow"`
	LineID          model.LineID `json:"line_id,omitempty"`
	AssignedAt      time.Time    `json:"assigned_at,omitempty"`
	FullLevelM      float64      `json:"full_level_m"`
	WaterLevelM     float64      `json:"water_level_m"`
	LevelSequence   uint64       `json:"level_sequence,omitempty"`
	LevelObservedAt time.Time    `json:"level_observed_at,omitempty"`
}

type EqualizationPool struct {
	mu     sync.Mutex
	basins map[string]Equalizer
}

func NewEqualizationPool(basins []Equalizer) (*EqualizationPool, error) {
	if len(basins) == 0 {
		return nil, errors.New("at least one equalizer is required")
	}
	pool := &EqualizationPool{basins: make(map[string]Equalizer, len(basins))}
	for _, basin := range basins {
		if basin.ID == "" || basin.Capacity <= 0 {
			return nil, errors.New("equalizer identity and capacity are required")
		}
		if basin.FullLevelM <= 0 {
			return nil, errors.New("equalizer full level is required")
		}
		pool.basins[basin.ID] = basin
	}
	return pool, nil
}

func (p *EqualizationPool) Assign(lineID model.LineID, flow float64, now time.Time) (Equalizer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	basin, err := p.resolveLocked(lineID, flow)
	if err != nil {
		return Equalizer{}, err
	}
	basin.AssignedFlow = flow
	if basin.LineID == "" {
		basin.LineID = lineID
	}
	basin.AssignedAt = now.UTC()
	p.basins[basin.ID] = basin
	return basin, nil
}

// Resolve picks the basin an assignment would use without changing the pool.
func (p *EqualizationPool) Resolve(lineID model.LineID, flow float64) (Equalizer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolveLocked(lineID, flow)
}

func (p *EqualizationPool) resolveLocked(lineID model.LineID, flow float64) (Equalizer, error) {
	for id, basin := range p.basins {
		if basin.LineID == lineID {
			return p.basins[id], nil
		}
	}
	ids := make([]string, 0, len(p.basins))
	for id := range p.basins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		basin := p.basins[id]
		if basin.LineID == "" && flow <= basin.Capacity {
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

// UpdateLevel records the latest water level reported for a basin. Levels at or
// above the full scale are accepted so the overflow guard can react to them.
func (p *EqualizationPool) UpdateLevel(basinID string, level float64, sequence uint64, observedAt time.Time) (Equalizer, error) {
	if math.IsNaN(level) || math.IsInf(level, 0) || level < 0 {
		return Equalizer{}, errors.New("water level is invalid")
	}
	if observedAt.IsZero() {
		return Equalizer{}, errors.New("water level timestamp is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	basin, ok := p.basins[basinID]
	if !ok {
		return Equalizer{}, errors.New("equalizer is not found")
	}
	basin.WaterLevelM = level
	basin.LevelSequence = sequence
	basin.LevelObservedAt = observedAt.UTC()
	p.basins[basinID] = basin
	return basin, nil
}

// FindByLine returns the basin currently assigned to a process line, if any.
func (p *EqualizationPool) FindByLine(lineID model.LineID) (Equalizer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, basin := range p.basins {
		if basin.LineID == lineID {
			return basin, true
		}
	}
	return Equalizer{}, false
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

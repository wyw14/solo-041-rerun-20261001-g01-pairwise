package api

import (
	"io"
	"net/http"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

type sampleRequest struct {
	LineID     string  `json:"line_id"`
	BasinID    string  `json:"basin_id"`
	Kind       string  `json:"kind"`
	Sequence   uint64  `json:"sequence"`
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
	ObservedAt string  `json:"observed_at"`
}

func (s *Server) receiveSample(w http.ResponseWriter, r *http.Request) {
	var input sampleRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	kind, err := model.ParseSampleKind(input.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	observedAt := s.plant.Clock()
	if input.ObservedAt != "" {
		observedAt, err = time.Parse(time.RFC3339, input.ObservedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	// Water level is reported by basin id ("按池编号上报") and handled by the
	// overflow protection book. The line is resolved from the current basin
	// assignment, so line_id is optional here. Other sample kinds keep the
	// strict per-line sequence cursor.
	if kind == model.SampleWaterLevel {
		status, reportErr := s.plant.ReportWaterLevel(r.Context(), input.BasinID, input.Sequence, input.Value, input.Unit, observedAt)
		if reportErr != nil {
			writeError(w, http.StatusConflict, reportErr)
			return
		}
		writeJSON(w, http.StatusAccepted, status)
		return
	}
	lineID, err := model.ParseLineID(input.LineID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sample := model.NewSample(lineID, input.BasinID, kind, input.Sequence, input.Value, input.Unit, observedAt)
	if err := s.plant.ReceiveSample(r.Context(), sample); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sample)
}

func (s *Server) labResult(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	outcome, err := s.plant.SubmitLabPayload(r.Context(), payload)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusAccepted, outcome)
}

package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) overflowList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"basins": s.plant.OverflowViews()})
}

func (s *Server) overflowDetail(w http.ResponseWriter, r *http.Request) {
	view, ok := s.plant.OverflowView(chi.URLParam(r, "basinID"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("equalizer is not found"))
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) overflowReset(w http.ResponseWriter, r *http.Request) {
	view, err := s.plant.ResetOverflow(chi.URLParam(r, "basinID"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

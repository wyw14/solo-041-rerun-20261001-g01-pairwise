package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// overflowProtection lists the overflow protection state of every intake /
// equalization basin, including the trigger basis and recovery conditions.
func (s *Server) overflowProtection(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"protection": s.plant.OverflowProtection()})
}

// overflowBasin shows the trigger basis and recovery condition for one basin.
func (s *Server) overflowBasin(w http.ResponseWriter, r *http.Request) {
	basinID := chi.URLParam(r, "basinID")
	status, err := s.plant.OverflowBasin(basinID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

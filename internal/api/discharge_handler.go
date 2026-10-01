package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-97/internal/model"
)

type dischargeFlowRequest struct {
	FlowM3Hour float64 `json:"flow_m3_hour"`
	Sequence   uint64  `json:"sequence"`
	ObservedAt string  `json:"observed_at"`
}

type dischargeFlowLimitRequest struct {
	LimitM3Hour float64 `json:"limit_m3_hour"`
}

func (s *Server) reportDischargeFlow(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input dischargeFlowRequest
	if err := decodeJSON(r, &input); err != nil {
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
	state, err := s.plant.ReportDischargeFlow(lineID, input.FlowM3Hour, input.Sequence, observedAt)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, state)
}

func (s *Server) resetDischargeFlowInterlock(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	state, err := s.plant.ResetDischargeFlowInterlock(lineID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) setDischargeFlowLimit(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input dischargeFlowLimitRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.plant.SetDischargeFlowLimit(lineID, input.LimitM3Hour); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	state, err := s.plant.FlowInterlock(lineID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) dischargeFlowInterlocks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": s.plant.FlowInterlocks(),
		"trips": s.plant.FlowInterlockTrips(),
	})
}

package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/model"
)

type flowReportRequest struct {
	Value      float64 `json:"value"`
	ObservedAt string  `json:"observed_at,omitempty"`
}

type flowConfigRequest struct {
	MaxFlow               *float64 `json:"max_flow,omitempty"`
	ContactTimeoutSeconds *int64   `json:"contact_timeout_seconds,omitempty"`
	TelemetryTTLSeconds   *int64   `json:"telemetry_ttl_seconds,omitempty"`
}

func (s *Server) dischargeFlowReport(w http.ResponseWriter, r *http.Request) {
	lineID, ok := parseDischargeLine(w, r)
	if !ok {
		return
	}
	var input flowReportRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	observedAt := s.plant.Clock()
	if input.ObservedAt != "" {
		parsed, err := time.Parse(time.RFC3339, input.ObservedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		observedAt = parsed
	}
	state, err := s.plant.ReportDischargeFlow(lineID, input.Value, observedAt)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusAccepted, state)
}

func (s *Server) dischargeFlowReset(w http.ResponseWriter, r *http.Request) {
	lineID, ok := parseDischargeLine(w, r)
	if !ok {
		return
	}
	state, err := s.plant.ResetDischargeFlow(lineID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) dischargeFlowConfig(w http.ResponseWriter, r *http.Request) {
	lineID, ok := parseDischargeLine(w, r)
	if !ok {
		return
	}
	var input flowConfigRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	current, err := s.plant.DischargeFlowStatus(lineID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	config := discharge.FlowConfig{
		MaxFlow:        current.Config.MaxFlow,
		ContactTimeout: time.Duration(current.Config.ContactTimeoutSeconds) * time.Second,
		TelemetryTTL:   time.Duration(current.Config.TelemetryTTLSeconds) * time.Second,
	}
	if input.MaxFlow != nil {
		config.MaxFlow = *input.MaxFlow
	}
	if input.ContactTimeoutSeconds != nil {
		config.ContactTimeout = time.Duration(*input.ContactTimeoutSeconds) * time.Second
	}
	if input.TelemetryTTLSeconds != nil {
		config.TelemetryTTL = time.Duration(*input.TelemetryTTLSeconds) * time.Second
	}
	state, err := s.plant.ConfigureDischargeFlow(lineID, config)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) dischargeFlowList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"lines": s.plant.DischargeFlowStatusAll()})
}

func parseDischargeLine(w http.ResponseWriter, r *http.Request) (model.LineID, bool) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return "", false
	}
	return lineID, true
}

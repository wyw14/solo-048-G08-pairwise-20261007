package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (a *App) reconnectStation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Epoch uint64 `json:"epoch"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := a.Reconnector.Reconnect(chi.URLParam(r, "stationID"), input.Epoch)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *App) beginZoneRecovery(w http.ResponseWriter, r *http.Request) {
	zone, err := a.ZoneRecovery.Begin(chi.URLParam(r, "zoneID"))
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, zone)
}

func (a *App) connectRecoveryStation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StationID string `json:"station_id"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	zone, err := a.ZoneRecovery.OnConnected(chi.URLParam(r, "zoneID"), input.StationID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	assessment, _, _ := a.ZoneRecovery.LatestAssessment(zone.ID)
	writeJSON(w, http.StatusOK, map[string]any{"zone": zone, "readiness": a.ZoneRecovery.Readiness(zone.ID), "assessment": assessment})
}

func (a *App) recordRecoveryProofs(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StationID    string  `json:"station_id"`
		ExchangerID  string  `json:"exchanger_id"`
		Revision     uint64  `json:"revision"`
		Outlet       float64 `json:"outlet_celsius"`
		PumpStage    int     `json:"pump_stage"`
		Differential float64 `json:"differential"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	warmup, err := a.Warmup.Proof(input.StationID, input.ExchangerID, input.Revision, input.Outlet)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	pressure, err := a.Pressure.Record(input.StationID, input.Revision, input.PumpStage, input.Differential)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	zone, err := a.ZoneRecovery.Evaluate(chi.URLParam(r, "zoneID"))
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	assessment, _, _ := a.ZoneRecovery.LatestAssessment(zone.ID)
	writeJSON(w, http.StatusOK, map[string]any{"warmup": warmup, "pressure": pressure, "zone": zone, "assessment": assessment})
}

// getZoneRecoveryAssessment returns the current recovery conclusion for one
// zone, recomputing it against live state and persisting the result.
func (a *App) getZoneRecoveryAssessment(w http.ResponseWriter, r *http.Request) {
	assessment, err := a.ZoneRecovery.Assessment(chi.URLParam(r, "zoneID"))
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, assessment)
}

// listRecoveryAssessments returns the persisted conclusion of every zone. It
// reads durable state only, so it stays available across restarts.
func (a *App) listRecoveryAssessments(w http.ResponseWriter, _ *http.Request) {
	assessments, err := a.ZoneRecovery.ListAssessments()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, assessments)
}

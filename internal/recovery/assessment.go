package recovery

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"heatpulse/internal/store"
)

// StationAssessment is the per-station verdict within a zone recovery run.
type StationAssessment struct {
	StationID   string    `json:"station_id"`
	ZoneID      string    `json:"zone_id"`
	Revision    uint64    `json:"revision"`
	Recoverable bool      `json:"recoverable"`
	Blockers    []string  `json:"blockers"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ZoneAssessment is the durable, queryable conclusion for one recovery revision.
type ZoneAssessment struct {
	ZoneID      string `json:"zone_id"`
	Revision    uint64 `json:"revision"`
	Phase       string `json:"phase"`
	Recoverable bool   `json:"recoverable"`
	// Blockers lists zone-wide conditions (currently boundary valves still
	// proven closed from isolation).
	Blockers  []string            `json:"blockers"`
	Stations  []StationAssessment `json:"stations"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// Blocker codes reported per station. They are stable strings so operators and
// the console can localize/explain them.
const (
	BlockerStationMissing    = "station_missing"
	BlockerNotReconnected    = "not_reconnected"
	BlockerStationOffline    = "station_offline"
	BlockerMaintenance       = "station_in_maintenance"
	BlockerWarmupStale       = "warmup_proof_stale_revision"
	BlockerWarmupMissing     = "warmup_proof_missing"
	BlockerWarmupNotReady    = "warmup_not_ready"
	BlockerPressureStale     = "pressure_proof_stale_revision"
	BlockerPressureMissing   = "pressure_proof_missing"
	BlockerPressureNotReady  = "pressure_not_ready"
	BlockerLatchHighReturn   = "latch_high_return"
	BlockerLatchFreezeRisk   = "latch_freeze_risk"
	BlockerLatchPressureTrip = "latch_pressure_trip"
	BlockerBoundaryClosed    = "boundary_valve_closed"
)

// AssessmentStore persists the latest assessment per zone through an atomic
// JSON document, so conclusions remain queryable across restarts.
type AssessmentStore struct {
	file *store.File[map[string]ZoneAssessment]
}

func NewAssessmentStore(directory string) *AssessmentStore {
	return &AssessmentStore{file: store.NewFile[map[string]ZoneAssessment](filepath.Join(directory, "recovery_assessments.json"))}
}

// Save replaces the stored assessment for its zone. Assessments from an older
// recovery revision are rejected so stale conclusions can never overwrite a
// newer run.
func (s *AssessmentStore) Save(candidate ZoneAssessment) (ZoneAssessment, error) {
	if candidate.ZoneID == "" {
		return ZoneAssessment{}, fmt.Errorf("zone id is required")
	}
	candidate.UpdatedAt = time.Now().UTC()
	sort.Slice(candidate.Stations, func(i, j int) bool {
		return candidate.Stations[i].StationID < candidate.Stations[j].StationID
	})
	_, err := s.file.Update(func(all *map[string]ZoneAssessment) error {
		if *all == nil {
			*all = make(map[string]ZoneAssessment)
		}
		if current, ok := (*all)[candidate.ZoneID]; ok && candidate.Revision < current.Revision {
			return fmt.Errorf("stale assessment revision %d for zone %s", candidate.Revision, candidate.ZoneID)
		}
		(*all)[candidate.ZoneID] = candidate
		return nil
	})
	if err != nil {
		return ZoneAssessment{}, err
	}
	return candidate, nil
}

// Get returns the latest persisted assessment for a zone.
func (s *AssessmentStore) Get(zoneID string) (ZoneAssessment, bool, error) {
	all, err := s.file.Load()
	if err != nil {
		return ZoneAssessment{}, false, err
	}
	assessment, ok := all[zoneID]
	return assessment, ok, nil
}

// List returns every persisted assessment ordered by zone id.
func (s *AssessmentStore) List() ([]ZoneAssessment, error) {
	all, err := s.file.Load()
	if err != nil {
		return nil, err
	}
	result := make([]ZoneAssessment, 0, len(all))
	for _, assessment := range all {
		result = append(result, assessment)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ZoneID < result[j].ZoneID })
	return result, nil
}

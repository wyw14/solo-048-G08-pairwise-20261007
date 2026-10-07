package recovery

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"heatpulse/internal/exchanger"
	"heatpulse/internal/network"
	"heatpulse/internal/protection"
	"heatpulse/internal/pump"
	"heatpulse/internal/store"
	"heatpulse/internal/valve"
)

const (
	BlockerNotConnected        = "not_connected"
	BlockerWarmupMissing       = "warmup_proof_missing"
	BlockerWarmupStale         = "warmup_proof_stale"
	BlockerWarmupNotReady      = "warmup_not_ready"
	BlockerPressureMissing     = "pressure_proof_missing"
	BlockerPressureStale       = "pressure_proof_stale"
	BlockerPressureNotReady    = "pressure_not_ready"
	BlockerProtectionLatched   = "protection_latch_active"
	BlockerBoundaryValveClosed = "boundary_valve_closed"
)

type Blocker struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

type StationAssessment struct {
	StationID    string    `json:"station_id"`
	Required     bool      `json:"required"`
	Connected    bool      `json:"connected"`
	Warm         bool      `json:"warm"`
	Pressurized  bool      `json:"pressurized"`
	LatchesClear bool      `json:"latches_clear"`
	Blockers     []Blocker `json:"blockers"`
}

type BoundaryValveStatus struct {
	ValveID   string `json:"valve_id"`
	SectionID string `json:"section_id"`
	Closed    bool   `json:"closed"`
}

type ZoneAssessment struct {
	ZoneID         string               `json:"zone_id"`
	Revision       uint64               `json:"revision"`
	Recoverable    bool                 `json:"recoverable"`
	Stations       []StationAssessment  `json:"stations"`
	BoundaryValves []BoundaryValveStatus `json:"boundary_valves"`
	Blockers       []Blocker            `json:"blockers"`
	AssessedAt     time.Time            `json:"assessed_at"`
}

// Record is a persisted assessment annotated against the zone's current
// recovery revision: once the revision moves on, the old conclusion is stale
// and must not be treated as the zone's present recoverability.
type Record struct {
	ZoneAssessment
	Stale bool `json:"stale"`
}

type Assessor struct {
	state      *network.State
	registry   *network.Registry
	topology   *network.Topology
	recovery   *ZoneRecovery
	warmup     *exchanger.WarmupService
	pressure   *pump.PressureMonitor
	boundaries *valve.BoundaryService
	latches    *protection.LatchStore
	persisted  *store.File[map[string]ZoneAssessment]
}

func NewAssessor(
	state *network.State,
	registry *network.Registry,
	topology *network.Topology,
	recovery *ZoneRecovery,
	warmup *exchanger.WarmupService,
	pressure *pump.PressureMonitor,
	boundaries *valve.BoundaryService,
	latches *protection.LatchStore,
	dataDirectory string,
) *Assessor {
	return &Assessor{
		state: state, registry: registry, topology: topology, recovery: recovery,
		warmup: warmup, pressure: pressure, boundaries: boundaries, latches: latches,
		persisted: store.NewFile[map[string]ZoneAssessment](filepath.Join(dataDirectory, "recovery_assessments.json")),
	}
}

// Assess evaluates the zone against its current recovery revision and persists
// the conclusion. Proofs recorded under an older revision are reported as
// stale and never count toward recoverability.
func (a *Assessor) Assess(zoneID string) (ZoneAssessment, error) {
	zone, ok := a.state.Get(zoneID)
	if !ok {
		return ZoneAssessment{}, fmt.Errorf("zone %s not found", zoneID)
	}
	assessment := ZoneAssessment{
		ZoneID: zoneID, Revision: zone.RecoveryRevision, Recoverable: true, AssessedAt: time.Now().UTC(),
	}
	for _, binding := range a.registry.ForZone(zoneID) {
		station := a.assessStation(zoneID, binding.StationID, binding.Required, zone.RecoveryRevision)
		assessment.Stations = append(assessment.Stations, station)
		if binding.Required && len(station.Blockers) > 0 {
			assessment.Recoverable = false
		}
	}
	for _, section := range a.topology.SectionsForZone(zoneID) {
		for _, valveID := range section.BoundaryValves {
			closed := a.boundaries.IsClosed(valveID)
			assessment.BoundaryValves = append(assessment.BoundaryValves, BoundaryValveStatus{
				ValveID: valveID, SectionID: section.ID, Closed: closed,
			})
			if closed {
				assessment.Blockers = append(assessment.Blockers, Blocker{Code: BlockerBoundaryValveClosed, Detail: valveID})
				assessment.Recoverable = false
			}
		}
	}
	if _, err := a.persisted.Update(func(all *map[string]ZoneAssessment) error {
		if *all == nil {
			*all = make(map[string]ZoneAssessment)
		}
		(*all)[zoneID] = assessment
		return nil
	}); err != nil {
		return ZoneAssessment{}, fmt.Errorf("persist recovery assessment: %w", err)
	}
	return assessment, nil
}

func (a *Assessor) assessStation(zoneID, stationID string, required bool, revision uint64) StationAssessment {
	result := StationAssessment{StationID: stationID, Required: required, LatchesClear: true}
	result.Connected = a.recovery.Connected(zoneID, stationID)
	if !result.Connected {
		result.Blockers = append(result.Blockers, Blocker{Code: BlockerNotConnected})
	}
	warmup, hasWarmup := a.warmup.Latest(stationID)
	switch {
	case !hasWarmup:
		result.Blockers = append(result.Blockers, Blocker{Code: BlockerWarmupMissing})
	case warmup.Revision != revision:
		result.Blockers = append(result.Blockers, Blocker{
			Code: BlockerWarmupStale, Detail: fmt.Sprintf("proof revision %d, current %d", warmup.Revision, revision),
		})
	case !warmup.Ready:
		result.Blockers = append(result.Blockers, Blocker{Code: BlockerWarmupNotReady, Detail: warmup.ExchangerID})
	}
	result.Warm = hasWarmup && warmup.Revision == revision && warmup.Ready
	pressure, hasPressure := a.pressure.Proof(stationID)
	switch {
	case !hasPressure:
		result.Blockers = append(result.Blockers, Blocker{Code: BlockerPressureMissing})
	case pressure.Revision != revision:
		result.Blockers = append(result.Blockers, Blocker{
			Code: BlockerPressureStale, Detail: fmt.Sprintf("proof revision %d, current %d", pressure.Revision, revision),
		})
	case !pressure.Ready:
		result.Blockers = append(result.Blockers, Blocker{Code: BlockerPressureNotReady})
	}
	result.Pressurized = hasPressure && pressure.Revision == revision && pressure.Ready
	latch := a.latches.Get(stationID)
	active := make([]string, 0, 3)
	if latch.HighReturn {
		active = append(active, "high_return")
	}
	if latch.FreezeRisk {
		active = append(active, "freeze_risk")
	}
	if latch.PressureTrip {
		active = append(active, "pressure_trip")
	}
	if len(active) > 0 {
		result.LatchesClear = false
		result.Blockers = append(result.Blockers, Blocker{Code: BlockerProtectionLatched, Detail: strings.Join(active, ",")})
	}
	return result
}

// Latest returns the persisted conclusion for a zone, flagged stale when the
// zone's recovery revision has moved past the assessed revision.
func (a *Assessor) Latest(zoneID string) (Record, bool, error) {
	zone, ok := a.state.Get(zoneID)
	if !ok {
		return Record{}, false, fmt.Errorf("zone %s not found", zoneID)
	}
	all, err := a.persisted.Load()
	if err != nil {
		return Record{}, false, err
	}
	assessment, ok := all[zoneID]
	if !ok {
		return Record{}, false, nil
	}
	return Record{ZoneAssessment: assessment, Stale: assessment.Revision != zone.RecoveryRevision}, true, nil
}

func (a *Assessor) List() ([]Record, error) {
	all, err := a.persisted.Load()
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(all))
	for zoneID, assessment := range all {
		stale := true
		if zone, ok := a.state.Get(zoneID); ok {
			stale = assessment.Revision != zone.RecoveryRevision
		}
		records = append(records, Record{ZoneAssessment: assessment, Stale: stale})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ZoneID < records[j].ZoneID })
	return records, nil
}

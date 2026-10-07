package recovery

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"heatpulse/internal/exchanger"
	"heatpulse/internal/network"
	"heatpulse/internal/protection"
	"heatpulse/internal/pump"
	"heatpulse/internal/station"
	"heatpulse/internal/valve"
)

type ZoneRecovery struct {
	state       *network.State
	registry    *network.Registry
	topology    *network.Topology
	stations    *station.Registry
	warmup      *exchanger.WarmupService
	pressure    *pump.PressureMonitor
	latches     *protection.LatchStore
	boundaries  *valve.BoundaryService
	assessments *AssessmentStore
	mu          sync.Mutex
	connected   map[string]map[string]bool
}

func NewZoneRecovery(
	state *network.State,
	registry *network.Registry,
	topology *network.Topology,
	stations *station.Registry,
	warmup *exchanger.WarmupService,
	pressure *pump.PressureMonitor,
	latches *protection.LatchStore,
	boundaries *valve.BoundaryService,
	assessments *AssessmentStore,
) *ZoneRecovery {
	return &ZoneRecovery{
		state: state, registry: registry, topology: topology, stations: stations,
		warmup: warmup, pressure: pressure, latches: latches, boundaries: boundaries,
		assessments: assessments, connected: make(map[string]map[string]bool),
	}
}

func (r *ZoneRecovery) Begin(zoneID string) (network.Zone, error) {
	r.mu.Lock()
	r.connected[zoneID] = make(map[string]bool)
	r.mu.Unlock()
	zone, err := r.state.Update(zoneID, func(zone *network.Zone) error {
		zone.RecoveryRevision++
		zone.Phase = network.PhaseWarming
		return nil
	})
	if err != nil {
		return zone, err
	}
	// A new recovery generation invalidates prior proofs and conclusions.
	if _, err := r.assess(zoneID); err != nil {
		return zone, err
	}
	return zone, nil
}

func (r *ZoneRecovery) OnConnected(zoneID, stationID string) (network.Zone, error) {
	r.mu.Lock()
	if r.connected[zoneID] == nil {
		r.connected[zoneID] = make(map[string]bool)
	}
	r.connected[zoneID][stationID] = true
	r.mu.Unlock()
	return r.evaluate(zoneID)
}

func (r *ZoneRecovery) Evaluate(zoneID string) (network.Zone, error) {
	return r.evaluate(zoneID)
}

func (r *ZoneRecovery) evaluate(zoneID string) (network.Zone, error) {
	assessment, err := r.assess(zoneID)
	if err != nil {
		zone, _ := r.state.Get(zoneID)
		return zone, err
	}
	zone, ok := r.state.Get(zoneID)
	if !ok {
		return network.Zone{}, fmt.Errorf("zone %s not found", zoneID)
	}
	if !assessment.Recoverable {
		return zone, nil
	}
	activated, err := r.state.Activate(zoneID, zone.RecoveryRevision)
	if err != nil {
		return activated, err
	}
	// Persist the final conclusion reflecting the active phase.
	if _, err := r.assess(zoneID); err != nil {
		return activated, err
	}
	return activated, nil
}

// Assessment recomputes and persists the recovery conclusion for a zone. It is
// safe to call at any time; the returned assessment reflects the current
// recovery generation and is durable across restarts.
func (r *ZoneRecovery) Assessment(zoneID string) (ZoneAssessment, error) {
	return r.assess(zoneID)
}

// LatestAssessment returns the most recently persisted conclusion without
// recomputing it.
func (r *ZoneRecovery) LatestAssessment(zoneID string) (ZoneAssessment, bool, error) {
	return r.assessments.Get(zoneID)
}

// ListAssessments returns every persisted zone conclusion.
func (r *ZoneRecovery) ListAssessments() ([]ZoneAssessment, error) {
	return r.assessments.List()
}

func (r *ZoneRecovery) assess(zoneID string) (ZoneAssessment, error) {
	zone, ok := r.state.Get(zoneID)
	if !ok {
		return ZoneAssessment{}, fmt.Errorf("zone %s not found", zoneID)
	}
	revision := zone.RecoveryRevision

	r.mu.Lock()
	connected := make(map[string]bool, len(r.connected[zoneID]))
	for stationID, isConnected := range r.connected[zoneID] {
		connected[stationID] = isConnected
	}
	r.mu.Unlock()

	stationAssessments := make([]StationAssessment, 0)
	allStationsReady := true
	for _, stationID := range r.registry.RequiredForZone(zoneID) {
		stationAssessment := r.assessStation(zoneID, stationID, revision, connected[stationID])
		if !stationAssessment.Recoverable {
			allStationsReady = false
		}
		stationAssessments = append(stationAssessments, stationAssessment)
	}
	sort.Slice(stationAssessments, func(i, j int) bool {
		return stationAssessments[i].StationID < stationAssessments[j].StationID
	})

	zoneBlockers := r.zoneBoundaryBlockers(zoneID)

	assessment := ZoneAssessment{
		ZoneID: zoneID, Revision: revision, Phase: string(zone.Phase),
		Recoverable: allStationsReady && len(zoneBlockers) == 0,
		Blockers:    zoneBlockers, Stations: stationAssessments,
	}
	saved, err := r.assessments.Save(assessment)
	if err != nil {
		return ZoneAssessment{}, err
	}
	return saved, nil
}

func (r *ZoneRecovery) assessStation(zoneID, stationID string, revision uint64, connected bool) StationAssessment {
	blockers := make([]string, 0)

	item, exists := r.stations.Get(stationID)
	if !exists {
		blockers = append(blockers, BlockerStationMissing)
		return r.stationVerdict(zoneID, stationID, revision, blockers)
	}
	if item.ZoneID != zoneID {
		blockers = append(blockers, BlockerStationMissing)
	}
	if !connected {
		blockers = append(blockers, BlockerNotReconnected)
	}
	if !item.Online {
		blockers = append(blockers, BlockerStationOffline)
	}
	if item.Maintenance {
		blockers = append(blockers, BlockerMaintenance)
	}

	// Warmup proof must belong to the current recovery generation. A proof
	// recorded under an older revision is stale and must not carry over.
	warmup, hasWarmup := r.warmup.Latest(stationID)
	switch {
	case !hasWarmup:
		blockers = append(blockers, BlockerWarmupMissing)
	case warmup.Revision != revision:
		blockers = append(blockers, BlockerWarmupStale)
	case !warmup.Ready:
		blockers = append(blockers, BlockerWarmupNotReady)
	}

	// Pump pressure proof is generation-scoped the same way.
	proof, hasPressure := r.pressure.Proof(stationID)
	switch {
	case !hasPressure:
		blockers = append(blockers, BlockerPressureMissing)
	case proof.Revision != revision:
		blockers = append(blockers, BlockerPressureStale)
	case !proof.Ready:
		blockers = append(blockers, BlockerPressureNotReady)
	}

	// Active protection latches hard-block recovery until they are reset.
	latches := r.latches.Get(stationID)
	if latches.HighReturn {
		blockers = append(blockers, BlockerLatchHighReturn)
	}
	if latches.FreezeRisk {
		blockers = append(blockers, BlockerLatchFreezeRisk)
	}
	if latches.PressureTrip {
		blockers = append(blockers, BlockerLatchPressureTrip)
	}

	return r.stationVerdict(zoneID, stationID, revision, blockers)
}

func (r *ZoneRecovery) stationVerdict(zoneID, stationID string, revision uint64, blockers []string) StationAssessment {
	return StationAssessment{
		StationID: stationID, ZoneID: zoneID, Revision: revision,
		Recoverable: len(blockers) == 0, Blockers: blockers,
		UpdatedAt: time.Now().UTC(),
	}
}

// zoneBoundaryBlockers reports every boundary valve of the zone that is still
// proven closed. A closed boundary means the section remains cut off from the
// network and the zone cannot be restored until the valve is reopened.
func (r *ZoneRecovery) zoneBoundaryBlockers(zoneID string) []string {
	valveIDs := make(map[string]struct{})
	for _, section := range r.topology.SectionsForZone(zoneID) {
		for _, valveID := range section.BoundaryValves {
			valveIDs[valveID] = struct{}{}
		}
	}
	closed := make([]string, 0)
	for valveID := range valveIDs {
		if r.boundaries.IsClosed(valveID) {
			closed = append(closed, BlockerBoundaryClosed+":"+valveID)
		}
	}
	sort.Strings(closed)
	return closed
}

func (r *ZoneRecovery) Readiness(zoneID string) map[string]map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	zone, _ := r.state.Get(zoneID)
	result := make(map[string]map[string]bool)
	for _, stationID := range r.registry.RequiredForZone(zoneID) {
		result[stationID] = map[string]bool{
			"connected":   r.connected[zoneID][stationID],
			"warm":        r.warmup.Ready(stationID, zone.RecoveryRevision),
			"pressurized": r.pressure.Ready(stationID, zone.RecoveryRevision),
		}
	}
	return result
}

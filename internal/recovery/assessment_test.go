package recovery

import (
	"context"
	"path/filepath"
	"testing"

	"heatpulse/internal/exchanger"
	"heatpulse/internal/network"
	"heatpulse/internal/protection"
	"heatpulse/internal/pump"
	"heatpulse/internal/station"
	"heatpulse/internal/store"
	"heatpulse/internal/valve"
)

type harness struct {
	dir      string
	state    *network.State
	registry *network.Registry
	topology *network.Topology
	stations *station.Registry
	warmup   *exchanger.WarmupService
	pressure *pump.PressureMonitor
	latches  *protection.LatchStore
	boundary *valve.BoundaryService
	assess   *AssessmentStore
	recovery *ZoneRecovery
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	state := network.NewState([]network.Zone{
		{ID: "z1", Name: "Zone 1", Phase: network.PhaseActive},
	})
	topology := network.NewTopology([]network.Section{
		{ID: "sec1", ZoneID: "z1", Upstream: "u", Downstream: "d",
			BoundaryValves: []string{"bv1", "bv2"}, ReliefValve: "rv1"},
	})
	if err := topology.Validate(); err != nil {
		t.Fatalf("topology: %v", err)
	}
	registry := network.NewRegistry([]network.StationBinding{
		{StationID: "s1", ZoneID: "z1", Required: true},
	})
	stations := station.NewRegistry([]station.Station{
		{ID: "s1", Name: "Station 1", ZoneID: "z1", Online: true,
			Exchangers: map[string]station.ExchangerPhase{"A": station.ExchangerCarrying}},
	})
	warmup := exchanger.NewWarmupService(stations)
	pressureMon := pump.NewPressureMonitor()
	snapshots := store.NewStationSnapshotStore(filepath.Join(dir, "snap"))
	latches := protection.NewLatchStore(snapshots)
	boundary := valve.NewBoundaryService()
	assess := NewAssessmentStore(dir)
	rec := NewZoneRecovery(state, registry, topology, stations, warmup, pressureMon, latches, boundary, assess)
	return &harness{
		dir: dir, state: state, registry: registry, topology: topology, stations: stations,
		warmup: warmup, pressure: pressureMon, latches: latches, boundary: boundary,
		assess: assess, recovery: rec,
	}
}

func (h *harness) recordReadyProofs(t *testing.T, revision uint64) {
	t.Helper()
	if _, err := h.warmup.Proof("s1", "A", revision, 42); err != nil {
		t.Fatalf("warmup proof: %v", err)
	}
	if _, err := h.pressure.Record("s1", revision, 2, 0.2); err != nil {
		t.Fatalf("pressure proof: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

func TestAssessment_AllClearActivatesAndPersists(t *testing.T) {
	h := newHarness(t)

	zone, err := h.recovery.Begin("z1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if zone.RecoveryRevision != 1 {
		t.Fatalf("revision = %d, want 1", zone.RecoveryRevision)
	}
	h.recordReadyProofs(t, 1)

	zone, err = h.recovery.OnConnected("z1", "s1")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if zone.Phase != network.PhaseActive {
		t.Fatalf("phase = %s, want active", zone.Phase)
	}

	assessment, ok, err := h.assess.Get("z1")
	if err != nil || !ok {
		t.Fatalf("persisted assessment: %v ok=%v", err, ok)
	}
	if !assessment.Recoverable {
		t.Fatalf("assessment not recoverable: %+v", assessment)
	}
	if len(assessment.Stations) != 1 || !assessment.Stations[0].Recoverable {
		t.Fatalf("station verdict wrong: %+v", assessment.Stations)
	}
	if len(assessment.Blockers) != 0 {
		t.Fatalf("unexpected zone blockers: %v", assessment.Blockers)
	}

	// Conclusion must remain queryable through a fresh store instance.
	reloaded := NewAssessmentStore(h.dir)
	fresh, ok, err := reloaded.Get("z1")
	if err != nil || !ok {
		t.Fatalf("reloaded assessment: %v ok=%v", err, ok)
	}
	if !fresh.Recoverable || fresh.Revision != 1 {
		t.Fatalf("reloaded assessment wrong: %+v", fresh)
	}
}

func TestAssessment_PerStationBlockers(t *testing.T) {
	h := newHarness(t)
	if _, err := h.recovery.Begin("z1"); err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Nothing reconnected, no proofs.
	assessment, err := h.recovery.Assessment("z1")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if assessment.Recoverable {
		t.Fatal("empty recovery must not be recoverable")
	}
	stationA := assessment.Stations[0]
	for _, want := range []string{
		BlockerNotReconnected, BlockerWarmupMissing, BlockerPressureMissing,
	} {
		if !contains(stationA.Blockers, want) {
			t.Errorf("missing blocker %q in %v", want, stationA.Blockers)
		}
	}

	// Protection latches hard-block recovery.
	if _, err := h.latches.Apply(protection.Latches{
		StationID: "s1", Revision: 1, HighReturn: true, FreezeRisk: true, PressureTrip: true,
	}); err != nil {
		t.Fatalf("latch: %v", err)
	}
	assessment, _ = h.recovery.Assessment("z1")
	stationA = assessment.Stations[0]
	for _, want := range []string{
		BlockerLatchHighReturn, BlockerLatchFreezeRisk, BlockerLatchPressureTrip,
	} {
		if !contains(stationA.Blockers, want) {
			t.Errorf("missing latch blocker %q in %v", want, stationA.Blockers)
		}
	}

	// Offline + maintenance also show up as distinct blockers.
	if _, err := h.latches.Reset("s1", 2); err != nil {
		t.Fatalf("reset latch: %v", err)
	}
	if _, err := h.stations.SetOnline("s1", false); err != nil {
		t.Fatalf("offline: %v", err)
	}
	if _, err := h.stations.Update("s1", func(s *station.Station) error { s.Maintenance = true; return nil }); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	assessment, _ = h.recovery.Assessment("z1")
	stationA = assessment.Stations[0]
	for _, want := range []string{BlockerStationOffline, BlockerMaintenance} {
		if !contains(stationA.Blockers, want) {
			t.Errorf("missing blocker %q in %v", want, stationA.Blockers)
		}
	}
}

func TestAssessment_BoundaryValveStillClosedBlocksZone(t *testing.T) {
	h := newHarness(t)
	if _, err := h.recovery.Begin("z1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	relief := valve.ReliefProof{ValveID: "rv1", SessionID: "session-1", Open: true}
	if _, err := h.boundary.Close(context.Background(), "bv1", "session-1", relief); err != nil {
		t.Fatalf("close boundary: %v", err)
	}
	assessment, err := h.recovery.Assessment("z1")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if assessment.Recoverable {
		t.Fatal("zone with closed boundary must not be recoverable")
	}
	if !contains(assessment.Blockers, BlockerBoundaryClosed+":bv1") {
		t.Fatalf("missing boundary blocker in %v", assessment.Blockers)
	}
}

func TestAssessment_NewRevisionInvalidatesOldProofs(t *testing.T) {
	h := newHarness(t)

	// First generation completes successfully.
	if _, err := h.recovery.Begin("z1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	h.recordReadyProofs(t, 1)
	if _, err := h.recovery.OnConnected("z1", "s1"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	first, _, _ := h.assess.Get("z1")
	if !first.Recoverable {
		t.Fatal("first generation should recover")
	}

	// Second generation: old proofs must no longer count.
	zone, err := h.recovery.Begin("z1")
	if err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if zone.RecoveryRevision != 2 {
		t.Fatalf("revision = %d, want 2", zone.RecoveryRevision)
	}
	assessment, err := h.recovery.Assessment("z1")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if assessment.Recoverable {
		t.Fatal("old-generation proofs must not make the new run recoverable")
	}
	stationA := assessment.Stations[0]
	for _, want := range []string{
		BlockerNotReconnected, BlockerWarmupStale, BlockerPressureStale,
	} {
		if !contains(stationA.Blockers, want) {
			t.Errorf("expected stale blocker %q in %v", want, stationA.Blockers)
		}
	}
	if contains(stationA.Blockers, BlockerWarmupMissing) || contains(stationA.Blockers, BlockerPressureMissing) {
		t.Errorf("old proofs should read as stale, not missing: %v", stationA.Blockers)
	}

	// Fresh proofs under the new generation, plus reconnect, clear it.
	h.recordReadyProofs(t, 2)
	if _, err := h.recovery.OnConnected("z1", "s1"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	second, _, _ := h.assess.Get("z1")
	if !second.Recoverable || second.Revision != 2 {
		t.Fatalf("second generation should recover, got %+v", second)
	}
}

func TestAssessmentStore_RejectsOlderRevision(t *testing.T) {
	dir := t.TempDir()
	s := NewAssessmentStore(dir)
	newer := ZoneAssessment{ZoneID: "z1", Revision: 5, Recoverable: true}
	if _, err := s.Save(newer); err != nil {
		t.Fatalf("save newer: %v", err)
	}
	if _, err := s.Save(ZoneAssessment{ZoneID: "z1", Revision: 4}); err == nil {
		t.Fatal("expected stale revision save to be rejected")
	}
	got, ok, err := s.Get("z1")
	if err != nil || !ok || got.Revision != 5 {
		t.Fatalf("stored assessment mutated: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestAssessment_List(t *testing.T) {
	h := newHarness(t)
	if _, err := h.recovery.Begin("z1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	listed, err := h.recovery.ListAssessments()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].ZoneID != "z1" {
		t.Fatalf("list wrong: %+v", listed)
	}
}

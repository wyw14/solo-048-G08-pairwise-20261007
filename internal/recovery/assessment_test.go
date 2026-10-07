package recovery_test

import (
	"context"
	"testing"

	"heatpulse/internal/exchanger"
	"heatpulse/internal/network"
	"heatpulse/internal/protection"
	"heatpulse/internal/pump"
	"heatpulse/internal/recovery"
	"heatpulse/internal/station"
	"heatpulse/internal/store"
	"heatpulse/internal/valve"
)

type fixture struct {
	assessor   *recovery.Assessor
	zone       *recovery.ZoneRecovery
	warmup     *exchanger.WarmupService
	pressure   *pump.PressureMonitor
	boundaries *valve.BoundaryService
	latches    *protection.LatchStore
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	state := network.NewState([]network.Zone{{ID: "north", Name: "北环", Phase: network.PhaseActive}})
	registry := network.NewRegistry([]network.StationBinding{
		{StationID: "alpha", ZoneID: "north", Required: true},
		{StationID: "beta", ZoneID: "north", Required: false},
	})
	topology := network.NewTopology([]network.Section{
		{ID: "north-loop", ZoneID: "north", Upstream: "north-supply", Downstream: "north-return", BoundaryValves: []string{"north-bv-a", "north-bv-b"}, ReliefValve: "north-rv"},
	})
	stations := station.NewRegistry([]station.Station{
		{ID: "alpha", Name: "甲站", ZoneID: "north", Online: true, Exchangers: map[string]station.ExchangerPhase{"A": station.ExchangerCarrying}},
		{ID: "beta", Name: "乙站", ZoneID: "north", Online: true, Exchangers: map[string]station.ExchangerPhase{"A": station.ExchangerCarrying}},
	})
	warmup := exchanger.NewWarmupService(stations)
	pressure := pump.NewPressureMonitor()
	boundaries := valve.NewBoundaryService()
	latches := protection.NewLatchStore(store.NewStationSnapshotStore(t.TempDir()))
	zone := recovery.NewZoneRecovery(state, registry, warmup, pressure)
	assessor := recovery.NewAssessor(state, registry, topology, zone, warmup, pressure, boundaries, latches, t.TempDir())
	return fixture{assessor: assessor, zone: zone, warmup: warmup, pressure: pressure, boundaries: boundaries, latches: latches}
}

func (f fixture) proveReady(t *testing.T, stationID string, revision uint64) {
	t.Helper()
	if _, err := f.zone.OnConnected("north", stationID); err != nil {
		t.Fatalf("connect %s: %v", stationID, err)
	}
	proof, err := f.warmup.Proof(stationID, "A", revision, 40)
	if err != nil || !proof.Ready {
		t.Fatalf("warmup proof %s: ready=%v err=%v", stationID, proof.Ready, err)
	}
	pressure, err := f.pressure.Record(stationID, revision, 2, 0.2)
	if err != nil || !pressure.Ready {
		t.Fatalf("pressure proof %s: ready=%v err=%v", stationID, pressure.Ready, err)
	}
}

func blockerCodes(blockers []recovery.Blocker) map[string]bool {
	codes := make(map[string]bool, len(blockers))
	for _, blocker := range blockers {
		codes[blocker.Code] = true
	}
	return codes
}

func stationByID(t *testing.T, assessment recovery.ZoneAssessment, stationID string) recovery.StationAssessment {
	t.Helper()
	for _, item := range assessment.Stations {
		if item.StationID == stationID {
			return item
		}
	}
	t.Fatalf("station %s missing from assessment", stationID)
	return recovery.StationAssessment{}
}

func TestAssessRecoverableWhenAllProofsCurrent(t *testing.T) {
	f := newFixture(t)
	if _, err := f.zone.Begin("north"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	f.proveReady(t, "alpha", 1)
	assessment, err := f.assessor.Assess("north")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if !assessment.Recoverable {
		t.Fatalf("expected recoverable, got blockers %+v / %+v", assessment.Blockers, stationByID(t, assessment, "alpha").Blockers)
	}
	if assessment.Revision != 1 {
		t.Fatalf("expected revision 1, got %d", assessment.Revision)
	}
	alpha := stationByID(t, assessment, "alpha")
	if !alpha.Connected || !alpha.Warm || !alpha.Pressurized || !alpha.LatchesClear {
		t.Fatalf("expected alpha fully ready, got %+v", alpha)
	}
	if len(assessment.BoundaryValves) != 2 {
		t.Fatalf("expected 2 boundary valves, got %+v", assessment.BoundaryValves)
	}
}

func TestAssessReportsMissingProofs(t *testing.T) {
	f := newFixture(t)
	if _, err := f.zone.Begin("north"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	assessment, err := f.assessor.Assess("north")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if assessment.Recoverable {
		t.Fatal("expected zone to be blocked")
	}
	codes := blockerCodes(stationByID(t, assessment, "alpha").Blockers)
	for _, want := range []string{recovery.BlockerNotConnected, recovery.BlockerWarmupMissing, recovery.BlockerPressureMissing} {
		if !codes[want] {
			t.Fatalf("expected blocker %s in %v", want, codes)
		}
	}
}

func TestStaleProofsDoNotSurviveRevisionChange(t *testing.T) {
	f := newFixture(t)
	if _, err := f.zone.Begin("north"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	f.proveReady(t, "alpha", 1)
	first, err := f.assessor.Assess("north")
	if err != nil || !first.Recoverable {
		t.Fatalf("expected recoverable at revision 1, got %+v err=%v", first, err)
	}
	if _, err := f.zone.Begin("north"); err != nil {
		t.Fatalf("re-begin: %v", err)
	}
	// Persisted conclusion from revision 1 is now stale.
	latest, ok, err := f.assessor.Latest("north")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if !latest.Stale {
		t.Fatal("expected persisted conclusion to be stale after revision change")
	}
	// Old proofs and the old connection must not count toward the new revision.
	second, err := f.assessor.Assess("north")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if second.Recoverable {
		t.Fatal("old proofs must not keep the zone recoverable after revision change")
	}
	codes := blockerCodes(stationByID(t, second, "alpha").Blockers)
	for _, want := range []string{recovery.BlockerNotConnected, recovery.BlockerWarmupStale, recovery.BlockerPressureStale} {
		if !codes[want] {
			t.Fatalf("expected blocker %s in %v", want, codes)
		}
	}
}

func TestProtectionLatchBlocksRecovery(t *testing.T) {
	f := newFixture(t)
	if _, err := f.zone.Begin("north"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	f.proveReady(t, "alpha", 1)
	if _, err := f.latches.Apply(protection.Latches{StationID: "alpha", Revision: 1, FreezeRisk: true}); err != nil {
		t.Fatalf("apply latch: %v", err)
	}
	assessment, err := f.assessor.Assess("north")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if assessment.Recoverable {
		t.Fatal("active latch must block recovery")
	}
	alpha := stationByID(t, assessment, "alpha")
	if alpha.LatchesClear || !blockerCodes(alpha.Blockers)[recovery.BlockerProtectionLatched] {
		t.Fatalf("expected protection_latch_active blocker, got %+v", alpha.Blockers)
	}
}

func TestClosedBoundaryValveBlocksRecovery(t *testing.T) {
	f := newFixture(t)
	if _, err := f.zone.Begin("north"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	f.proveReady(t, "alpha", 1)
	relief := valve.ReliefProof{ValveID: "north-rv", SessionID: "s1", Open: true}
	if _, err := f.boundaries.Close(context.Background(), "north-bv-a", "s1", relief); err != nil {
		t.Fatalf("close boundary: %v", err)
	}
	assessment, err := f.assessor.Assess("north")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if assessment.Recoverable {
		t.Fatal("closed boundary valve must block recovery")
	}
	if !blockerCodes(assessment.Blockers)[recovery.BlockerBoundaryValveClosed] {
		t.Fatalf("expected boundary_valve_closed zone blocker, got %+v", assessment.Blockers)
	}
}

func TestNonRequiredStationDoesNotBlock(t *testing.T) {
	f := newFixture(t)
	if _, err := f.zone.Begin("north"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	f.proveReady(t, "alpha", 1)
	assessment, err := f.assessor.Assess("north")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if !assessment.Recoverable {
		t.Fatal("non-required station beta must not block the zone")
	}
	beta := stationByID(t, assessment, "beta")
	if beta.Required || len(beta.Blockers) == 0 {
		t.Fatalf("expected beta non-required with blockers listed, got %+v", beta)
	}
}

func TestAssessmentPersistedAndQueryable(t *testing.T) {
	f := newFixture(t)
	if _, err := f.zone.Begin("north"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	f.proveReady(t, "alpha", 1)
	if _, err := f.assessor.Assess("north"); err != nil {
		t.Fatalf("assess: %v", err)
	}
	latest, ok, err := f.assessor.Latest("north")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if latest.Stale || !latest.Recoverable || latest.Revision != 1 {
		t.Fatalf("unexpected persisted record: %+v", latest)
	}
	records, err := f.assessor.List()
	if err != nil || len(records) != 1 || records[0].ZoneID != "north" {
		t.Fatalf("unexpected list: %+v err=%v", records, err)
	}
}

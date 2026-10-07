package recovery

import (
	"fmt"
	"sync"

	"heatpulse/internal/exchanger"
	"heatpulse/internal/network"
	"heatpulse/internal/pump"
)

type ZoneRecovery struct {
	state     *network.State
	registry  *network.Registry
	warmup    *exchanger.WarmupService
	pressure  *pump.PressureMonitor
	mu        sync.Mutex
	connected map[string]map[string]bool
}

func NewZoneRecovery(state *network.State, registry *network.Registry, warmup *exchanger.WarmupService, pressure *pump.PressureMonitor) *ZoneRecovery {
	return &ZoneRecovery{state: state, registry: registry, warmup: warmup, pressure: pressure, connected: make(map[string]map[string]bool)}
}

func (r *ZoneRecovery) Begin(zoneID string) (network.Zone, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected[zoneID] = make(map[string]bool)
	return r.state.Update(zoneID, func(zone *network.Zone) error {
		zone.RecoveryRevision++
		zone.Phase = network.PhaseWarming
		return nil
	})
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
	r.mu.Lock()
	defer r.mu.Unlock()
	zone, ok := r.state.Get(zoneID)
	if !ok {
		return network.Zone{}, fmt.Errorf("zone %s not found", zoneID)
	}
	for _, stationID := range r.registry.RequiredForZone(zoneID) {
		if !r.connected[zoneID][stationID] || !r.warmup.Ready(stationID, zone.RecoveryRevision) || !r.pressure.Ready(stationID, zone.RecoveryRevision) {
			return zone, nil
		}
	}
	return r.state.Activate(zoneID, zone.RecoveryRevision)
}

func (r *ZoneRecovery) Connected(zoneID, stationID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connected[zoneID][stationID]
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

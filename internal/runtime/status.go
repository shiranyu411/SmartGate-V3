package runtime

import (
	"time"

	"smartgate/internal/smartnode"
	"smartgate/internal/state"
)

const (
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusRescue   = "rescue"
	StatusStopped  = "stopped"
)

type Status struct {
	Phase            string                 `json:"phase"`
	ListenAddr       string                 `json:"listen_addr"`
	ConfigEpoch      int64                  `json:"config_epoch"`
	TopologyHash     string                 `json:"topology_hash"`
	GatewayMode      string                 `json:"gateway_mode,omitempty"`
	RescueActive     bool                   `json:"rescue_active"`
	RescueReason     string                 `json:"rescue_reason,omitempty"`
	RecoveryError    string                 `json:"recovery_error,omitempty"`
	LegacyMigration  *LegacyMigrationStatus `json:"legacy_migration,omitempty"`
	UpdatedAt        *time.Time             `json:"updated_at,omitempty"`
	Workers          []WorkerSummary        `json:"workers"`
	Bindings         []state.PoolBinding    `json:"bindings"`
	CutoversInFlight []CutoverProgress      `json:"cutovers_in_flight"`
	Drains           []DrainProgress        `json:"drains"`
}

// LegacyMigrationStatus summarizes the durable pre-SmartNode binding
// migration report so dropped bindings stay visible after the upgrade.
type LegacyMigrationStatus struct {
	MigratedAt time.Time                      `json:"migrated_at"`
	Resolved   int                            `json:"resolved"`
	Unresolved []state.LegacyBindingReference `json:"unresolved"`
	Warnings   []string                       `json:"warnings"`
}

func legacyMigrationStatus(report state.LegacyBindingMigrationReport) *LegacyMigrationStatus {
	status := &LegacyMigrationStatus{
		MigratedAt: report.MigratedAt, Resolved: len(report.Resolved),
		Unresolved: append([]state.LegacyBindingReference{}, report.Unresolved...),
		Warnings:   append([]string{}, report.Warnings...),
	}
	return status
}

type WorkerSummary struct {
	ID    string           `json:"id"`
	Core  smartnode.CoreID `json:"core"`
	State string           `json:"state"`
	Pools []string         `json:"pools"`
}

type CutoverProgress struct {
	Pool  string `json:"pool"`
	Stage string `json:"stage"`
}

type DrainProgress struct {
	Pool          string     `json:"pool"`
	OldGeneration uint64     `json:"old_generation"`
	Remaining     int        `json:"remaining"`
	Deadline      *time.Time `json:"deadline,omitempty"`
	Status        string     `json:"status"`
	LastError     string     `json:"last_error,omitempty"`
	RetryCount    int        `json:"retry_count"`
}

func NewStatus(listenAddr string, runtimeState state.RuntimeState) Status {
	status := Status{
		Phase:         StatusRunning,
		ListenAddr:    listenAddr,
		ConfigEpoch:   runtimeState.ConfigEpoch,
		TopologyHash:  runtimeState.TopologyHash,
		GatewayMode:   runtimeState.GatewayMode,
		RescueActive:  runtimeState.RescueActive,
		RescueReason:  runtimeState.RescueReason,
		RecoveryError: runtimeState.RecoveryError,
		Workers:       []WorkerSummary{}, Bindings: []state.PoolBinding{},
		CutoversInFlight: []CutoverProgress{}, Drains: []DrainProgress{},
	}
	if runtimeState.RescueActive {
		status.Phase = StatusRescue
	}
	now := time.Now()
	status.UpdatedAt = &now
	return status
}

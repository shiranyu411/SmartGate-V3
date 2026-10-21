package state

import (
	"time"

	"smartgate/internal/smartnode"
)

type RuntimeState struct {
	ConfigEpoch    int64                           `json:"config_epoch"`
	TopologyHash   string                          `json:"topology_hash,omitempty"`
	GatewayMode    string                          `json:"gateway_mode,omitempty"`
	RescueActive   bool                            `json:"rescue_active"`
	RescueReason   string                          `json:"rescue_reason,omitempty"`
	RecoveryError  string                          `json:"recovery_error,omitempty"`
	CommitInFlight *CommitRef                      `json:"commit_in_flight,omitempty"`
	LastAppliedAt  *time.Time                      `json:"last_applied_at,omitempty"`
	LastRollbackAt *time.Time                      `json:"last_rollback_at,omitempty"`
	LastRestoreAt  *time.Time                      `json:"last_restore_at,omitempty"`
	Scheduler      map[string]SchedulerTaskRuntime `json:"scheduler,omitempty"`
	ProxySelection map[string]string               `json:"proxy_selection,omitempty"`
	NodeHealth     map[string]NodeHealth           `json:"node_health,omitempty"`
	ProviderLogs   []ProviderRefreshLog            `json:"provider_logs,omitempty"`
}

type CommitRef struct {
	OpID         string    `json:"op_id"`
	StartedAt    time.Time `json:"started_at"`
	TopologyHash string    `json:"topology_hash,omitempty"`
}

type SchedulerTaskRuntime struct {
	LastRun       *time.Time `json:"last_run,omitempty"`
	LastStatus    string     `json:"last_status,omitempty"`
	LastOutput    string     `json:"last_output,omitempty"`
	NextRun       *time.Time `json:"next_run,omitempty"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	ClaimDeadline *time.Time `json:"claim_deadline,omitempty"`
}

type NodeHealth struct {
	LastTest            *time.Time        `json:"last_test,omitempty"`
	Status              string            `json:"status,omitempty"`
	DelayMS             int               `json:"delay_ms,omitempty"`
	Error               string            `json:"error,omitempty"`
	TestURL             string            `json:"test_url,omitempty"`
	UpdatedAt           *time.Time        `json:"updated_at,omitempty"`
	ConsecutiveFailures int               `json:"consecutive_failures,omitempty"`
	WatchdogStatus      string            `json:"watchdog_status,omitempty"`
	WatchdogUpdatedAt   *time.Time        `json:"watchdog_updated_at,omitempty"`
	WatchdogContext     NodeHealthContext `json:"watchdog_context"`
	Context             NodeHealthContext `json:"context"`
}

type NodeHealthContext struct {
	Revision int              `json:"revision"`
	Core     smartnode.CoreID `json:"core,omitempty"`
	Worker   string           `json:"worker,omitempty"`
	Method   string           `json:"method"`
}

type ProviderRefreshLog struct {
	Time       time.Time `json:"time"`
	Scope      string    `json:"scope"`
	ProviderID string    `json:"provider_id,omitempty"`
	Status     string    `json:"status"`
	Changed    bool      `json:"changed"`
	Count      int       `json:"count,omitempty"`
	Error      string    `json:"error,omitempty"`
}

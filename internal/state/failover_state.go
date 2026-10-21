package state

import (
	"errors"
	"os"
	"time"
)

const DefaultFailoverCooldownSeconds = 60

type FailoverState struct {
	Enabled         bool                         `json:"enabled"`
	CooldownSeconds int                          `json:"cooldown_seconds"`
	Locks           map[string]FailoverLock      `json:"locks,omitempty"`
	Pools           map[string]FailoverPoolState `json:"pools,omitempty"`
}

type FailoverLock struct {
	Reason   string    `json:"reason,omitempty"`
	LockedAt time.Time `json:"locked_at"`
}

type FailoverPoolState struct {
	PoolID        string     `json:"pool_id"`
	Role          string     `json:"role"`
	Current       string     `json:"current,omitempty"`
	Previous      string     `json:"previous,omitempty"`
	LastKind      string     `json:"last_kind,omitempty"`
	LastReason    string     `json:"last_reason,omitempty"`
	LastChangedAt *time.Time `json:"last_changed_at,omitempty"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	Terminal      bool       `json:"terminal"`
}

type FailoverStateStore struct {
	layout Layout
}

func NewFailoverStateStore(layout Layout) FailoverStateStore {
	return FailoverStateStore{layout: layout}
}

func DefaultFailoverState() FailoverState {
	return FailoverState{
		Enabled:         true,
		CooldownSeconds: DefaultFailoverCooldownSeconds,
		Locks:           map[string]FailoverLock{},
		Pools:           map[string]FailoverPoolState{},
	}
}

func NormalizeFailoverState(state FailoverState) FailoverState {
	if state.CooldownSeconds <= 0 {
		state.CooldownSeconds = DefaultFailoverCooldownSeconds
	}
	if state.Locks == nil {
		state.Locks = map[string]FailoverLock{}
	}
	if state.Pools == nil {
		state.Pools = map[string]FailoverPoolState{}
	}
	return state
}

func (s FailoverStateStore) Load() (FailoverState, error) {
	var failover FailoverState
	err := loadJSON(s.layout.FailoverStatePath(), &failover)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultFailoverState(), nil
	}
	if err != nil {
		return FailoverState{}, err
	}
	return NormalizeFailoverState(failover), nil
}

func (s FailoverStateStore) Save(failover FailoverState) error {
	failover = NormalizeFailoverState(failover)
	_, err := saveJSONAtomicIfChanged(s.layout.FailoverStatePath(), failover, 0o600)
	return err
}

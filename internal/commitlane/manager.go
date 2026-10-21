package commitlane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/netgate"
	"smartgate/internal/opid"
	"smartgate/internal/state"
)

var ErrRescueActive = errors.New("rescue gate is active")
var ErrRecoveryRequired = errors.New("durable transaction recovery is required; call POST /api/runtime/recover")

type Manager struct {
	locks  *lockset.LockSet
	layout state.Layout
	owners *netgate.OwnerRegistry
}

func New(locks *lockset.LockSet, layout state.Layout, owners *netgate.OwnerRegistry) *Manager {
	return &Manager{locks: locks, layout: layout, owners: owners}
}

type CommitRequest struct {
	OpID         string
	Kind         string
	TopologyHash string
	StartedAt    time.Time
}

type CommitResult struct {
	OpID         string
	Kind         string
	TopologyHash string
	StartedAt    time.Time
	CompletedAt  time.Time
}

func (m *Manager) WithCommit(ctx context.Context, req CommitRequest, fn func(*lockset.Session) error) (*CommitResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	safeOpID, err := opid.Clean(req.OpID)
	if err != nil {
		return nil, err
	}
	req.OpID = safeOpID
	session := m.locks.Session(req.OpID)

	if canRunDuringRescue(req.Kind) {
		releaseRescue, err := session.AcquireContext(ctx, lockset.LevelRescueGate)
		if err != nil {
			return nil, err
		}
		defer releaseRescue()
		return m.runCommit(ctx, req, session, fn)
	}

	releaseRescue, err := session.AcquireContext(ctx, lockset.LevelRescueGate)
	if err != nil {
		return nil, err
	}
	if m.rescueActive() {
		releaseRescue()
		return nil, ErrRescueActive
	}
	releaseRescue()

	return m.runCommit(ctx, req, session, func(session *lockset.Session) error {
		if m.rescueActive() {
			return ErrRescueActive
		}
		return fn(session)
	})
}

func (m *Manager) runCommit(ctx context.Context, req CommitRequest, session *lockset.Session, fn func(*lockset.Session) error) (*CommitResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	releaseCommit, err := session.AcquireContext(ctx, lockset.LevelCommitLane)
	if err != nil {
		return nil, err
	}
	defer releaseCommit()
	if req.Kind != "recovery" && req.Kind != "rescue" {
		journals, err := state.NewTransactionStore(m.layout).List()
		if err != nil {
			return nil, err
		}
		if len(journals) > 0 {
			return nil, fmt.Errorf("%w: transaction %s (%s) is %s", ErrRecoveryRequired, journals[0].OpID, journals[0].Kind, journals[0].Phase)
		}
	}
	if err := fn(session); err != nil {
		return nil, err
	}
	return &CommitResult{
		OpID:         req.OpID,
		Kind:         req.Kind,
		TopologyHash: req.TopologyHash,
		StartedAt:    req.StartedAt,
		CompletedAt:  time.Now(),
	}, nil
}

func (m *Manager) WithRescueGate(ctx context.Context, opID string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	safeOpID, err := opid.Clean(opID)
	if err != nil {
		return err
	}
	session := m.locks.Session(safeOpID)
	releaseRescue, err := session.AcquireContext(ctx, lockset.LevelRescueGate)
	if err != nil {
		return err
	}
	defer releaseRescue()
	return fn()
}

func (m *Manager) Layout() state.Layout {
	return m.layout
}

func (m *Manager) Owners() *netgate.OwnerRegistry {
	return m.owners
}

func (m *Manager) rescueActive() bool {
	_, err := os.Stat(m.layout.RescueStatusPath())
	return err == nil
}

func canRunDuringRescue(kind string) bool {
	switch kind {
	case "rescue", "recovery":
		return true
	default:
		return false
	}
}

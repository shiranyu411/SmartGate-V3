package commitlane

import (
	"context"
	"fmt"
	"time"

	"smartgate/internal/lockset"
	"smartgate/internal/provider"
	"smartgate/internal/state"
)

type Recovery struct {
	commit    *Manager
	store     *state.Store
	snapshots *state.SnapshotStore
}

func NewRecovery(commit *Manager, store *state.Store, snapshots *state.SnapshotStore) *Recovery {
	return &Recovery{commit: commit, store: store, snapshots: snapshots}
}

func (r *Recovery) Recover(ctx context.Context) error {
	if err := r.recoverDurableJournals(ctx); err != nil {
		return err
	}
	runtimeState, err := r.store.LoadRuntime()
	if err != nil {
		return err
	}
	if runtimeState.CommitInFlight == nil {
		return nil
	}
	if journal, loadErr := state.NewTransactionStore(r.commit.Layout()).Load(runtimeState.CommitInFlight.OpID); loadErr == nil && (isConfigTransactionKind(journal.Kind) || journal.Kind == "selector_apply") {
		// Config transactions do not use the legacy preimage-only recovery
		// path. Their journal contains the durable applied-state source and is
		// reconciled by ConfigTxn after data-plane/OpenWrt adapters are created.
		return nil
	}

	_, err = r.commit.WithCommit(ctx, CommitRequest{
		OpID:      runtimeState.CommitInFlight.OpID + "-recover",
		Kind:      "recovery",
		StartedAt: time.Now(),
	}, func(session *lockset.Session) error {
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		defer releaseStore()

		releaseSnapshot, err := session.Acquire(lockset.LevelSnapshot)
		if err != nil {
			return err
		}
		defer releaseSnapshot()

		releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
		if err != nil {
			return err
		}
		defer releaseRuntime()

		env, err := r.snapshots.LoadPreimage(runtimeState.CommitInFlight.OpID)
		if err != nil {
			return err
		}
		if _, err := r.store.SaveSpec(env.Spec); err != nil {
			return err
		}
		if env.LastGoodCaptured || env.LastGood != nil {
			if _, err := provider.NewLastGoodStore(r.commit.Layout()).ReplaceAll(ctx, lastGoodSnapshotUpdates(env.LastGood)); err != nil {
				return err
			}
		}
		env.Runtime.CommitInFlight = nil
		now := time.Now()
		env.Runtime.LastRollbackAt = &now
		if err := r.store.SaveRuntime(env.Runtime); err != nil {
			return err
		}
		if err := r.snapshots.RemovePreimage(runtimeState.CommitInFlight.OpID); err != nil {
			return err
		}
		return nil
	})
	return err
}

func (r *Recovery) recoverDurableJournals(ctx context.Context) error {
	transactions := state.NewTransactionStore(r.commit.Layout())
	journals, err := transactions.List()
	if err != nil {
		return err
	}
	for _, journal := range journals {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch journal.Kind {
		case "provider_apply", "business_pool_apply", "smartnode_core_choice":
		case "config_apply", "snapshot_rollback", "backup_restore", "selector_apply":
			// These transactions require data-plane and OpenWrt adapters, which are
			// created after the state bootstrap. ConfigTxn.Recover handles them
			// before the HTTP server starts listening.
			continue
		default:
			return fmt.Errorf("transaction %s requires data-plane recovery for kind %s", journal.OpID, journal.Kind)
		}
		if err := r.recoverDurableJournal(ctx, transactions, journal); err != nil {
			return err
		}
	}
	return nil
}

func (r *Recovery) recoverDurableJournal(ctx context.Context, transactions *state.TransactionStore, journal state.TransactionJournal) error {
	_, err := r.commit.WithCommit(ctx, CommitRequest{
		OpID:      journal.OpID + "-recover",
		Kind:      "recovery",
		StartedAt: time.Now(),
	}, func(session *lockset.Session) error {
		releaseStore, err := session.Acquire(lockset.LevelStore)
		if err != nil {
			return err
		}
		defer releaseStore()
		releaseSnapshot, err := session.Acquire(lockset.LevelSnapshot)
		if err != nil {
			return err
		}
		defer releaseSnapshot()
		releaseRuntime, err := session.Acquire(lockset.LevelRuntime)
		if err != nil {
			return err
		}
		defer releaseRuntime()

		if journal.Phase != state.TransactionCommitted {
			if _, err := r.store.SaveSpec(journal.Source.Spec); err != nil {
				return err
			}
			if journal.Source.LastGoodCaptured {
				if _, err := provider.NewLastGoodStore(r.commit.Layout()).ReplaceAll(ctx, lastGoodSnapshotUpdates(journal.Source.LastGood)); err != nil {
					return err
				}
			}
			runtimeState := journal.Source.Runtime
			runtimeState.CommitInFlight = nil
			now := time.Now().UTC()
			runtimeState.LastRollbackAt = &now
			if err := r.store.SaveRuntime(runtimeState); err != nil {
				return err
			}
		}
		if err := r.snapshots.RemovePreimage(journal.OpID); err != nil {
			return err
		}
		return transactions.Remove(journal.OpID)
	})
	return err
}

func lastGoodSnapshotUpdates(snapshots []state.LastGoodSnapshot) []provider.LastGoodUpdate {
	updates := make([]provider.LastGoodUpdate, 0, len(snapshots))
	for _, snapshot := range snapshots {
		updates = append(updates, provider.LastGoodUpdate{
			ProviderID: snapshot.ProviderID, Subscription: snapshot.Subscription,
			Identities: snapshot.Identities, Outbounds: snapshot.Outbounds,
		})
	}
	return updates
}

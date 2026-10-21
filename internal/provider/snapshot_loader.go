package provider

import (
	"fmt"
	"os"

	"smartgate/internal/state"
)

type SnapshotLastGoodLoader struct {
	byProvider map[string]LastGoodEnvelope
}

func NewSnapshotLastGoodLoader(snapshots []state.LastGoodSnapshot) (*SnapshotLastGoodLoader, error) {
	loader := &SnapshotLastGoodLoader{byProvider: make(map[string]LastGoodEnvelope, len(snapshots))}
	for _, snapshot := range snapshots {
		if snapshot.ProviderID == "" {
			return nil, fmt.Errorf("last-good snapshot provider id is required")
		}
		if _, exists := loader.byProvider[snapshot.ProviderID]; exists {
			return nil, fmt.Errorf("duplicate last-good snapshot for provider %s", snapshot.ProviderID)
		}
		count := len(snapshot.Identities)
		if count == 0 {
			count = len(snapshot.Outbounds)
		}
		loader.byProvider[snapshot.ProviderID] = LastGoodEnvelope{
			ProviderID:   snapshot.ProviderID,
			Subscription: snapshot.Subscription,
			Identities:   snapshot.Identities,
			Outbounds:    snapshot.Outbounds,
			Count:        count,
		}
	}
	return loader, nil
}

func (l *SnapshotLastGoodLoader) Load(providerID string) (LastGoodEnvelope, error) {
	env, ok := l.byProvider[providerID]
	if !ok {
		return LastGoodEnvelope{}, os.ErrNotExist
	}
	return env, nil
}

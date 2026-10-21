package commitlane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/selection"
	"smartgate/internal/state"
)

type blockingSelectorControl struct {
	*selectorControlFake
	entered chan<- struct{}
	release <-chan struct{}
}

func (control *blockingSelectorControl) Select(ctx context.Context, selector, target string) error {
	control.entered <- struct{}{}
	<-control.release
	return control.selectorControlFake.Select(ctx, selector, target)
}

func legacySelectorLastGood() []state.LastGoodSnapshot {
	return []state.LastGoodSnapshot{{
		ProviderID: "legacy-provider",
		Outbounds: []map[string]any{{
			"type": "vmess", "tag": "legacy-node", "server": "legacy.example", "server_port": 443, "uuid": "legacy-test-secret",
		}},
	}}
}

func saveLegacyAppliedLastGood(t *testing.T, store *state.Store) {
	t.Helper()
	applied, err := store.LoadAppliedState()
	if err != nil {
		t.Fatal(err)
	}
	applied.LastGood = legacySelectorLastGood()
	if err := store.SaveAppliedState(applied); err != nil {
		t.Fatal(err)
	}
}

func assertJournalContainsLegacyLastGood(t *testing.T, layout state.Layout, opID string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(layout.TransactionsDir(), opID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"outbounds"`) {
		t.Fatalf("legacy journal fixture does not contain outbounds: %s", body)
	}
}

func assertJournalLastGoodIdentityOnly(t *testing.T, layout state.Layout, opID string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(layout.TransactionsDir(), opID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"outbounds"`) {
		t.Fatalf("journal rewrote legacy outbounds: %s", body)
	}
	journal, err := state.NewTransactionStore(layout).Load(opID)
	if err != nil {
		t.Fatal(err)
	}
	promoted := 0
	for name, snapshots := range map[string][]state.LastGoodSnapshot{
		"source":         journal.Source.LastGood,
		"source applied": journal.Source.Applied.LastGood,
		"target":         journal.Target.LastGood,
		"target applied": journal.Target.Applied.LastGood,
	} {
		if len(snapshots) == 0 {
			continue
		}
		if len(snapshots) != 1 || len(snapshots[0].Identities) != 1 || snapshots[0].Identities[0].RawTag != "legacy-node" || len(snapshots[0].Outbounds) != 0 {
			t.Fatalf("%s last-good was not identity-only: %+v", name, snapshots)
		}
		promoted++
	}
	if promoted == 0 {
		t.Fatalf("journal contains no promoted last-good: %+v", journal)
	}
}

func TestSelectorTxnNewJournalNormalizesLegacyAppliedLastGood(t *testing.T) {
	defer selection.SetSelectorRebindDelaysForTest(nil)()
	txn, store, layout, baseControl := selectorTxnFixture(t)
	saveLegacyAppliedLastGood(t, store)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	txn.control = &blockingSelectorControl{selectorControlFake: baseControl, entered: entered, release: release}
	done := make(chan error, 1)
	go func() {
		done <- txn.Apply(context.Background(), SelectorTxnApplyRequest{
			OpID: "selector-legacy-applied", PoolID: "general", Selector: "Pool_general", Source: "node-a", Target: "node-b",
			TargetPool: state.FailoverPoolState{PoolID: "general", Previous: "node-a", Current: "node-b"}, ExpectedConfigEpoch: 2,
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("selector did not reach live mutation after writing its journal")
	}
	assertJournalLastGoodIdentityOnly(t, layout, "selector-legacy-applied")
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("selector did not finish after release")
	}
}

func TestCutoverTxnNewJournalNormalizesLegacyAppliedLastGood(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreA, commitCoreB)
	saveLegacyAppliedLastGood(t, fixture.store)
	targetID := fixture.nodes["ai-target"].ID
	fixture.faults.failVerifyAt["ai:"+targetID] = 1
	stopEntered := make(chan struct{}, 1)
	stopRelease := make(chan struct{})
	fixture.faults.stopEntered = stopEntered
	fixture.faults.stopRelease = stopRelease
	done := make(chan error, 1)
	go func() {
		_, err := fixture.txn.Cutover(context.Background(), CutoverRequest{
			OpID: "cutover-legacy-applied", PoolID: "ai", TargetSmartNode: targetID,
			Source: CutoverSource{SmartNode: fixture.bindings["ai"].Target.SmartNode, Generation: fixture.bindings["ai"].Generation}, ExpectedConfigEpoch: 7,
		})
		done <- err
	}()
	select {
	case <-stopEntered:
	case <-time.After(time.Second):
		t.Fatal("cutover did not reach rollback after writing its journal")
	}
	assertJournalLastGoodIdentityOnly(t, fixture.layout, "cutover-legacy-applied")
	close(stopRelease)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected injected cutover failure")
		}
	case <-time.After(time.Second):
		t.Fatal("cutover did not finish after release")
	}
}

func TestSelectorTxnRecoveryFailureRewritesLegacyLastGoodIdentityOnly(t *testing.T) {
	txn, _, layout, control := selectorTxnFixture(t)
	source, err := txn.loadState()
	if err != nil {
		t.Fatal(err)
	}
	source.LastGoodCaptured = true
	source.LastGood = legacySelectorLastGood()
	source.Applied.LastGood = legacySelectorLastGood()
	target := cloneSelectorState(source)
	target.Runtime.ProxySelection["general"] = "node-b"
	journal := state.TransactionJournal{
		OpID: "selector-legacy-recovery", Kind: selectorTransactionKind, Phase: state.TransactionCommitted,
		PoolID: "general", Selector: "Pool_general", Source: source, Target: target,
	}
	if err := state.NewTransactionStore(layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	assertJournalContainsLegacyLastGood(t, layout, journal.OpID)
	control.fail = errors.New("injected selector recovery failure")
	if err := txn.Recover(context.Background()); err == nil {
		t.Fatal("expected selector recovery failure")
	}
	assertJournalLastGoodIdentityOnly(t, layout, journal.OpID)
}

func TestCutoverTxnRecoveryFailureRewritesLegacyLastGoodIdentityOnly(t *testing.T) {
	fixture := newCutoverTestFixture(t, state.CutoverStrict, commitCoreB, commitCoreB)
	source, err := fixture.txn.loadState()
	if err != nil {
		t.Fatal(err)
	}
	source.LastGoodCaptured = true
	source.LastGood = legacySelectorLastGood()
	source.Applied.LastGood = legacySelectorLastGood()
	target := cloneSelectorState(source)
	journal := state.TransactionJournal{
		OpID: "cutover-legacy-recovery", Kind: selectorTransactionKind, Phase: state.TransactionApplying,
		PoolID: "ai", CutoverPolicy: state.CutoverStrict, Source: source, Target: target,
	}
	if err := state.NewTransactionStore(fixture.layout).Save(journal); err != nil {
		t.Fatal(err)
	}
	assertJournalContainsLegacyLastGood(t, fixture.layout, journal.OpID)
	movedRuntimeDir := fixture.layout.RuntimeDir + "-before-recovery"
	if err := os.Rename(fixture.layout.RuntimeDir, movedRuntimeDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.layout.RuntimeDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.txn.Recover(context.Background()); err == nil {
		t.Fatal("expected cutover recovery persistence failure")
	}
	assertJournalLastGoodIdentityOnly(t, fixture.layout, journal.OpID)
}

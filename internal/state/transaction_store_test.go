package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTransactionStoreRejectsOversizedJournalBeforeDecode(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.TransactionsDir(), "oversized.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), MaxTransactionJournalBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewTransactionStore(layout).Load("oversized")
	var limitErr *JSONResourceLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("oversized journal was decoded: %T %v", err, err)
	}
}

func TestTransactionStoreRoundTripListAndRemove(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewTransactionStore(layout)
	journal := TransactionJournal{
		OpID:       "restore-1",
		Kind:       "snapshot_restore",
		Phase:      TransactionPrepared,
		StartedAt:  time.Now().UTC().Add(-time.Minute),
		SourceHash: "old",
		TargetHash: "new",
		Source:     TransactionState{Spec: DefaultSpecState()},
		Target:     TransactionState{Spec: DefaultSpecState()},
	}
	if err := store.Save(journal); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(journal.OpID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != TransactionJournalSchemaVersion || got.Phase != TransactionPrepared || got.SourceHash != "old" {
		t.Fatalf("unexpected journal: %+v", got)
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].OpID != journal.OpID {
		t.Fatalf("unexpected journals: %+v", items)
	}
	if err := store.Remove(journal.OpID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(journal.OpID); !os.IsNotExist(err) {
		t.Fatalf("expected removed journal, got %v", err)
	}
}

func TestTransactionStoreRejectsUnsafeIDAndPhase(t *testing.T) {
	layout := Layout{DataDir: t.TempDir(), RuntimeDir: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	store := NewTransactionStore(layout)
	if err := store.Save(TransactionJournal{OpID: "../escape", Kind: "restore", Phase: TransactionPrepared}); err == nil {
		t.Fatal("expected unsafe op id to fail")
	}
	if err := store.Save(TransactionJournal{OpID: "restore-1", Kind: "restore", Phase: "unknown"}); err == nil {
		t.Fatal("expected invalid phase to fail")
	}
}
